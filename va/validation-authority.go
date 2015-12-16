// Copyright 2014 ISRG.  All rights reserved
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package va

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/cactus/go-statsd-client/statsd"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/jmhodges/clock"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/letsencrypt/net/publicsuffix"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/miekg/dns"
	"github.com/letsencrypt/boulder/probs"

	"github.com/letsencrypt/boulder/bdns"
	"github.com/letsencrypt/boulder/core"
	blog "github.com/letsencrypt/boulder/log"
)

const maxRedirect = 10
const whitespaceCutset = "\n\t "

var validationTimeout = time.Second * 5

// ValidationAuthorityImpl represents a VA
type ValidationAuthorityImpl struct {
	RA           core.RegistrationAuthority
	log          *blog.AuditLogger
	DNSResolver  bdns.DNSResolver
	IssuerDomain string
	SafeBrowsing SafeBrowsing
	httpPort     int
	httpsPort    int
	tlsPort      int
	UserAgent    string
	stats        statsd.Statter
	clk          clock.Clock
}

// PortConfig specifies what ports the VA should call to on the remote
// host when performing its checks.
type PortConfig struct {
	HTTPPort  int
	HTTPSPort int
	TLSPort   int
}

// NewValidationAuthorityImpl constructs a new VA
func NewValidationAuthorityImpl(pc *PortConfig, sbc SafeBrowsing, stats statsd.Statter, clk clock.Clock) *ValidationAuthorityImpl {
	logger := blog.GetAuditLogger()
	logger.Notice("Validation Authority Starting")
	return &ValidationAuthorityImpl{
		SafeBrowsing: sbc,
		log:          logger,
		httpPort:     pc.HTTPPort,
		httpsPort:    pc.HTTPSPort,
		tlsPort:      pc.TLSPort,
		stats:        stats,
		clk:          clk,
	}
}

// Used for audit logging
type verificationRequestEvent struct {
	ID           string         `json:",omitempty"`
	Requester    int64          `json:",omitempty"`
	Challenge    core.Challenge `json:",omitempty"`
	RequestTime  time.Time      `json:",omitempty"`
	ResponseTime time.Time      `json:",omitempty"`
	Error        string         `json:",omitempty"`
}

// getAddr will query for all A records associated with hostname and return the
// prefered address, the first net.IP in the addrs slice, and all addresses resolved.
// This is the same choice made by the Go internal resolution library used by
// net/http, except we only send A queries and accept IPv4 addresses.
// TODO(#593): Add IPv6 support
func (va ValidationAuthorityImpl) getAddr(hostname string) (addr net.IP, addrs []net.IP, problem *probs.ProblemDetails) {
	addrs, err := va.DNSResolver.LookupHost(hostname)
	if err != nil {
		problem = bdns.ProblemDetailsFromDNSError(err)
		va.log.Debug(fmt.Sprintf("%s DNS failure: %s", hostname, err))
		return
	}

	if len(addrs) == 0 {
		problem = &probs.ProblemDetails{
			Type:   probs.UnknownHostProblem,
			Detail: fmt.Sprintf("No IPv4 addresses found for %s", hostname),
		}
		return
	}
	addr = addrs[0]
	va.log.Info(fmt.Sprintf("Resolved addresses for %s [using %s]: %s", hostname, addr, addrs))
	return
}

type dialer struct {
	record core.ValidationRecord
}

func (d *dialer) Dial(_, _ string) (net.Conn, error) {
	realDialer := net.Dialer{Timeout: validationTimeout}
	return realDialer.Dial("tcp", net.JoinHostPort(d.record.AddressUsed.String(), d.record.Port))
}

// resolveAndConstructDialer gets the prefered address using va.getAddr and returns
// the chosen address and dialer for that address and correct port.
func (va *ValidationAuthorityImpl) resolveAndConstructDialer(name string, port int) (dialer, *probs.ProblemDetails) {
	d := dialer{
		record: core.ValidationRecord{
			Hostname: name,
			Port:     strconv.Itoa(port),
		},
	}

	addr, allAddrs, err := va.getAddr(name)
	if err != nil {
		return d, err
	}
	d.record.AddressesResolved = allAddrs
	d.record.AddressUsed = addr
	return d, nil
}

// Validation methods

func (va *ValidationAuthorityImpl) fetchHTTP(identifier core.AcmeIdentifier, path string, useTLS bool, input core.Challenge) ([]byte, []core.ValidationRecord, *probs.ProblemDetails) {
	challenge := input

	host := identifier.Value
	scheme := "http"
	port := va.httpPort
	if useTLS {
		scheme = "https"
		port = va.httpsPort
	}

	urlHost := host
	if !((scheme == "http" && port == 80) ||
		(scheme == "https" && port == 443)) {
		urlHost = net.JoinHostPort(host, strconv.Itoa(port))
	}

	url := &url.URL{
		Scheme: scheme,
		Host:   urlHost,
		Path:   path,
	}

	// AUDIT[ Certificate Requests ] 11917fa4-10ef-4e0d-9105-bacbe7836a3c
	va.log.Audit(fmt.Sprintf("Attempting to validate %s for %s", challenge.Type, url))
	httpRequest, err := http.NewRequest("GET", url.String(), nil)
	if err != nil {
		va.log.Debug(fmt.Sprintf("%s [%s] HTTP failure: %s", challenge.Type, identifier, err))
		return nil, nil, &probs.ProblemDetails{
			Type:   probs.MalformedProblem,
			Detail: "URL provided for HTTP was invalid",
		}
	}

	if va.UserAgent != "" {
		httpRequest.Header["User-Agent"] = []string{va.UserAgent}
	}

	dialer, prob := va.resolveAndConstructDialer(host, port)
	dialer.record.URL = url.String()
	validationRecords := []core.ValidationRecord{dialer.record}
	if prob != nil {
		return nil, validationRecords, prob
	}

	tr := &http.Transport{
		// We are talking to a client that does not yet have a certificate,
		// so we accept a temporary, invalid one.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		// We don't expect to make multiple requests to a client, so close
		// connection immediately.
		DisableKeepAlives: true,
		// Intercept Dial in order to connect to the IP address we
		// select.
		Dial: dialer.Dial,
	}

	// Some of our users use mod_security. Mod_security sees a lack of Accept
	// headers as bot behavior and rejects requests. While this is a bug in
	// mod_security's rules (given that the HTTP specs disagree with that
	// requirement), we add the Accept header now in order to fix our
	// mod_security users' mysterious breakages. See
	// <https://github.com/SpiderLabs/owasp-modsecurity-crs/issues/265> and
	// <https://github.com/letsencrypt/boulder/issues/1019>. This was done
	// because it's a one-line fix with no downside. We're not likely to want to
	// do many more things to satisfy misunderstandings around HTTP.
	httpRequest.Header.Set("Accept", "*/*")

	logRedirect := func(req *http.Request, via []*http.Request) error {
		if len(validationRecords) >= maxRedirect {
			return fmt.Errorf("Too many redirects")
		}

		// Set Accept header for mod_security (see the other place the header is
		// set)
		req.Header.Set("Accept", "*/*")
		if va.UserAgent != "" {
			req.Header["User-Agent"] = []string{va.UserAgent}
		}

		reqHost := req.URL.Host
		var reqPort int
		if h, p, err := net.SplitHostPort(reqHost); err == nil {
			reqHost = h
			reqPort, err = strconv.Atoi(p)
			if err != nil {
				return err
			}
			if reqPort <= 0 || reqPort > 65535 {
				return fmt.Errorf("Invalid port number %d in redirect", reqPort)
			}
		} else if strings.ToLower(req.URL.Scheme) == "https" {
			reqPort = 443
		} else {
			reqPort = 80
		}

		dialer, err := va.resolveAndConstructDialer(reqHost, reqPort)
		dialer.record.URL = req.URL.String()
		validationRecords = append(validationRecords, dialer.record)
		if err != nil {
			return err
		}
		tr.Dial = dialer.Dial
		va.log.Info(fmt.Sprintf("%s [%s] redirect from %q to %q [%s]", challenge.Type, identifier, via[len(via)-1].URL.String(), req.URL.String(), dialer.record.AddressUsed))
		return nil
	}
	client := http.Client{
		Transport:     tr,
		CheckRedirect: logRedirect,
		Timeout:       validationTimeout,
	}
	httpResponse, err := client.Do(httpRequest)
	if err != nil {
		va.log.Debug(err.Error())
		return nil, validationRecords, &probs.ProblemDetails{
			Type:   parseHTTPConnError(err),
			Detail: fmt.Sprintf("Could not connect to %s", url),
		}
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode != 200 {
		return nil, validationRecords, &probs.ProblemDetails{
			Type: probs.UnauthorizedProblem,
			Detail: fmt.Sprintf("Invalid response from %s [%s]: %d",
				url.String(), dialer.record.AddressUsed, httpResponse.StatusCode),
		}
	}

	body, err := ioutil.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, validationRecords, &probs.ProblemDetails{
			Type:   probs.UnauthorizedProblem,
			Detail: fmt.Sprintf("Error reading HTTP response body: %v", err),
		}
	}
	return body, validationRecords, nil
}

func (va *ValidationAuthorityImpl) validateTLSWithZName(identifier core.AcmeIdentifier, challenge core.Challenge, zName string) ([]core.ValidationRecord, *probs.ProblemDetails) {
	addr, allAddrs, problem := va.getAddr(identifier.Value)
	validationRecords := []core.ValidationRecord{
		core.ValidationRecord{
			Hostname:          identifier.Value,
			AddressesResolved: allAddrs,
			AddressUsed:       addr,
		},
	}
	if problem != nil {
		return validationRecords, problem
	}

	// Make a connection with SNI = nonceName
	portString := strconv.Itoa(va.tlsPort)
	hostPort := net.JoinHostPort(addr.String(), portString)
	validationRecords[0].Port = portString
	va.log.Notice(fmt.Sprintf("%s [%s] Attempting to validate for %s %s", challenge.Type, identifier, hostPort, zName))
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: validationTimeout}, "tcp", hostPort, &tls.Config{
		ServerName:         zName,
		InsecureSkipVerify: true,
	})

	if err != nil {
		va.log.Debug(fmt.Sprintf("%s [%s] TLS Connection failure: %s", challenge.Type, identifier, err))
		return validationRecords, &probs.ProblemDetails{
			Type:   parseHTTPConnError(err),
			Detail: "Failed to connect to host for DVSNI challenge",
		}
	}
	defer conn.Close()

	// Check that zName is a dNSName SAN in the server's certificate
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return validationRecords, &probs.ProblemDetails{
			Type:   probs.UnauthorizedProblem,
			Detail: "No certs presented for TLS SNI challenge",
		}
	}
	for _, name := range certs[0].DNSNames {
		if subtle.ConstantTimeCompare([]byte(name), []byte(zName)) == 1 {
			return validationRecords, nil
		}
	}

	return validationRecords, &probs.ProblemDetails{
		Type: probs.UnauthorizedProblem,
		Detail: fmt.Sprintf("Correct zName not found for TLS SNI challenge. Found '%v'",
			strings.Join(certs[0].DNSNames, ", ")),
	}
}

func (va *ValidationAuthorityImpl) validateHTTP01(identifier core.AcmeIdentifier, challenge core.Challenge) ([]core.ValidationRecord, *probs.ProblemDetails) {
	if identifier.Type != core.IdentifierDNS {
		va.log.Debug(fmt.Sprintf("%s [%s] Identifier failure", challenge.Type, identifier))
		return nil, &probs.ProblemDetails{
			Type:   probs.MalformedProblem,
			Detail: "Identifier type for HTTP validation was not DNS",
		}
	}

	// Perform the fetch
	path := fmt.Sprintf(".well-known/acme-challenge/%s", challenge.Token)
	body, validationRecords, err := va.fetchHTTP(identifier, path, false, challenge)
	if err != nil {
		return validationRecords, err
	}

	payload := strings.TrimRight(string(body), whitespaceCutset)

	// Parse body as a key authorization object
	serverKeyAuthorization, authErr := core.NewKeyAuthorizationFromString(payload)
	if authErr != nil {
		va.log.Debug(authErr.Error())
		return validationRecords, &probs.ProblemDetails{
			Type:   probs.UnauthorizedProblem,
			Detail: fmt.Sprintf("Error parsing key authorization file: %s", authErr.Error()),
		}
	}

	// Check that the account key for this challenge is authorized by this object
	if !serverKeyAuthorization.Match(challenge.Token, challenge.AccountKey) {
		errString := fmt.Sprintf("The key authorization file from the server did not match this challenge [%v] != [%v]",
			challenge.KeyAuthorization.String(), string(body))
		va.log.Debug(errString)
		return validationRecords, &probs.ProblemDetails{
			Type:   probs.UnauthorizedProblem,
			Detail: errString,
		}
	}

	return validationRecords, nil
}

func (va *ValidationAuthorityImpl) validateTLSSNI01(identifier core.AcmeIdentifier, challenge core.Challenge) ([]core.ValidationRecord, *probs.ProblemDetails) {
	if identifier.Type != "dns" {
		va.log.Debug(fmt.Sprintf("TLS-SNI [%s] Identifier failure", identifier))
		return nil, &probs.ProblemDetails{
			Type:   probs.MalformedProblem,
			Detail: "Identifier type for TLS-SNI was not DNS",
		}
	}

	// Compute the digest that will appear in the certificate
	h := sha256.New()
	h.Write([]byte(challenge.KeyAuthorization.String()))
	Z := hex.EncodeToString(h.Sum(nil))
	ZName := fmt.Sprintf("%s.%s.%s", Z[:32], Z[32:], core.TLSSNISuffix)

	return va.validateTLSWithZName(identifier, challenge, ZName)
}

// parseHTTPConnError returns the ACME ProblemType corresponding to an error
// that occurred during domain validation.
func parseHTTPConnError(err error) probs.ProblemType {
	if urlErr, ok := err.(*url.Error); ok {
		err = urlErr.Err
	}

	// XXX: On all of the resolvers I tested that validate DNSSEC, there is
	// no differentation between a DNSSEC failure and an unknown host. If we
	// do not verify DNSSEC ourselves, this function should be modified.
	if netErr, ok := err.(*net.OpError); ok {
		dnsErr, ok := netErr.Err.(*net.DNSError)
		if ok && !dnsErr.Timeout() && !dnsErr.Temporary() {
			return probs.UnknownHostProblem
		} else if fmt.Sprintf("%T", netErr.Err) == "tls.alert" {
			return probs.TLSProblem
		}
	}

	return probs.ConnectionProblem
}

func (va *ValidationAuthorityImpl) validateDNS01(identifier core.AcmeIdentifier, challenge core.Challenge) ([]core.ValidationRecord, *probs.ProblemDetails) {
	if identifier.Type != core.IdentifierDNS {
		va.log.Debug(fmt.Sprintf("DNS [%s] Identifier failure", identifier))
		return nil, &probs.ProblemDetails{
			Type:   probs.MalformedProblem,
			Detail: "Identifier type for DNS was not itself DNS",
		}
	}

	// Compute the digest of the key authorization file
	h := sha256.New()
	h.Write([]byte(challenge.KeyAuthorization.String()))
	authorizedKeysDigest := hex.EncodeToString(h.Sum(nil))

	// Look for the required record in the DNS
	challengeSubdomain := fmt.Sprintf("%s.%s", core.DNSPrefix, identifier.Value)
	txts, err := va.DNSResolver.LookupTXT(challengeSubdomain)

	if err != nil {
		va.log.Debug(fmt.Sprintf("%s [%s] DNS failure: %s", challenge.Type, identifier, err))
		return nil, bdns.ProblemDetailsFromDNSError(err)
	}

	for _, element := range txts {
		if subtle.ConstantTimeCompare([]byte(element), []byte(authorizedKeysDigest)) == 1 {
			// Successful challenge validation
			return nil, nil
		}
	}

	return nil, &probs.ProblemDetails{
		Type:   probs.UnauthorizedProblem,
		Detail: "Correct value not found for DNS challenge",
	}
}

func (va *ValidationAuthorityImpl) checkCAA(identifier core.AcmeIdentifier, regID int64) *probs.ProblemDetails {
	// Check CAA records for the requested identifier
	present, valid, err := va.CheckCAARecords(identifier)
	if err != nil {
		va.log.Warning(fmt.Sprintf("Problem checking CAA: %s", err))
		return bdns.ProblemDetailsFromDNSError(err)
	}
	// AUDIT[ Certificate Requests ] 11917fa4-10ef-4e0d-9105-bacbe7836a3c
	va.log.Audit(fmt.Sprintf("Checked CAA records for %s, registration ID %d [Present: %t, Valid for issuance: %t]", identifier.Value, regID, present, valid))
	if !valid {
		return &probs.ProblemDetails{
			Type:   probs.ConnectionProblem,
			Detail: "CAA check for identifier failed",
		}
	}
	return nil
}

// Overall validation process

func (va *ValidationAuthorityImpl) validate(authz core.Authorization, challengeIndex int) {
	logEvent := verificationRequestEvent{
		ID:          authz.ID,
		Requester:   authz.RegistrationID,
		RequestTime: va.clk.Now(),
	}
	challenge := &authz.Challenges[challengeIndex]
	vStart := va.clk.Now()
	validationRecords, prob := va.validateChallengeAndCAA(authz.Identifier, *challenge, authz.RegistrationID)
	va.stats.TimingDuration(fmt.Sprintf("VA.Validations.%s.%s", challenge.Type, challenge.Status), time.Since(vStart), 1.0)

	challenge.ValidationRecord = validationRecords
	if prob != nil {
		challenge.Status = core.StatusInvalid
		challenge.Error = prob
		logEvent.Error = prob.Error()
	} else if !authz.Challenges[challengeIndex].RecordsSane() {
		challenge.Status = core.StatusInvalid
		challenge.Error = &probs.ProblemDetails{Type: probs.ServerInternalProblem,
			Detail: "Records for validation failed sanity check"}
		logEvent.Error = challenge.Error.Error()
	} else {
		challenge.Status = core.StatusValid
	}
	logEvent.Challenge = *challenge

	// AUDIT[ Certificate Requests ] 11917fa4-10ef-4e0d-9105-bacbe7836a3c
	va.log.AuditObject("Validation result", logEvent)

	va.log.Notice(fmt.Sprintf("Validations: %+v", authz))

	va.RA.OnValidationUpdate(authz)
}

func (va *ValidationAuthorityImpl) validateChallengeAndCAA(identifier core.AcmeIdentifier, challenge core.Challenge, regID int64) ([]core.ValidationRecord, *probs.ProblemDetails) {
	ch := make(chan *probs.ProblemDetails, 1)
	go func() {
		ch <- va.checkCAA(identifier, regID)
	}()

	validationRecords, err := va.validateChallenge(identifier, challenge)
	if err != nil {
		return validationRecords, err
	}

	caaProblem := <-ch
	if caaProblem != nil {
		return validationRecords, caaProblem
	}
	return validationRecords, nil
}

func (va *ValidationAuthorityImpl) validateChallenge(identifier core.AcmeIdentifier, challenge core.Challenge) ([]core.ValidationRecord, *probs.ProblemDetails) {
	if !challenge.IsSane(true) {
		return nil, &probs.ProblemDetails{
			Type:   probs.MalformedProblem,
			Detail: fmt.Sprintf("Challenge failed sanity check."),
		}
	}
	switch challenge.Type {
	case core.ChallengeTypeHTTP01:
		return va.validateHTTP01(identifier, challenge)
	case core.ChallengeTypeTLSSNI01:
		return va.validateTLSSNI01(identifier, challenge)
	case core.ChallengeTypeDNS01:
		return va.validateDNS01(identifier, challenge)
	}
	return nil, &probs.ProblemDetails{
		Type:   probs.MalformedProblem,
		Detail: fmt.Sprintf("invalid challenge type %s", challenge.Type),
	}
}

// UpdateValidations runs the validate() method asynchronously using goroutines.
func (va *ValidationAuthorityImpl) UpdateValidations(authz core.Authorization, challengeIndex int) error {
	go va.validate(authz, challengeIndex)
	return nil
}

// CAASet consists of filtered CAA records
type CAASet struct {
	Issue     []*dns.CAA
	Issuewild []*dns.CAA
	Iodef     []*dns.CAA
	Unknown   []*dns.CAA
}

// returns true if any CAA records have unknown tag properties and are flagged critical.
func (caaSet CAASet) criticalUnknown() bool {
	if len(caaSet.Unknown) > 0 {
		for _, caaRecord := range caaSet.Unknown {
			// Critical flag is 1, but according to RFC 6844 any flag other than
			// 0 should currently be interpreted as critical.
			if caaRecord.Flag > 0 {
				return true
			}
		}
	}

	return false
}

// Filter CAA records by property
func newCAASet(CAAs []*dns.CAA) *CAASet {
	var filtered CAASet

	for _, caaRecord := range CAAs {
		switch caaRecord.Tag {
		case "issue":
			filtered.Issue = append(filtered.Issue, caaRecord)
		case "issuewild":
			filtered.Issuewild = append(filtered.Issuewild, caaRecord)
		case "iodef":
			filtered.Iodef = append(filtered.Iodef, caaRecord)
		default:
			filtered.Unknown = append(filtered.Unknown, caaRecord)
		}
	}

	return &filtered
}

func (va *ValidationAuthorityImpl) getCAASet(hostname string) (*CAASet, error) {
	hostname = strings.TrimRight(hostname, ".")
	labels := strings.Split(hostname, ".")
	// See RFC 6844 "Certification Authority Processing" for pseudocode.
	// Essentially: check CAA records for the FDQN to be issued, and all parent
	// domains.
	// We depend on our resolver to snap CNAME and DNAME records.
	for i := 0; i < len(labels); i++ {
		name := strings.Join(labels[i:len(labels)], ".")
		// Break if we've reached an ICANN TLD.
		if tld, err := publicsuffix.ICANNTLD(name); err != nil || tld == name {
			break
		}
		CAAs, err := va.DNSResolver.LookupCAA(name)
		if err != nil {
			return nil, err
		}
		if len(CAAs) > 0 {
			return newCAASet(CAAs), nil
		}
	}
	// no CAA records found
	return nil, nil
}

// CheckCAARecords verifies that, if the indicated subscriber domain has any CAA
// records, they authorize the configured CA domain to issue a certificate
func (va *ValidationAuthorityImpl) CheckCAARecords(identifier core.AcmeIdentifier) (present, valid bool, err error) {
	hostname := strings.ToLower(identifier.Value)
	caaSet, err := va.getCAASet(hostname)
	if err != nil {
		return
	}
	if caaSet == nil {
		// No CAA records found, can issue
		present = false
		valid = true
		return
	} else if caaSet.criticalUnknown() {
		present = true
		valid = false
		return
	} else if len(caaSet.Issue) > 0 || len(caaSet.Issuewild) > 0 {
		present = true
		var checkSet []*dns.CAA
		if strings.SplitN(hostname, ".", 2)[0] == "*" {
			checkSet = caaSet.Issuewild
		} else {
			checkSet = caaSet.Issue
		}
		for _, caa := range checkSet {
			if caa.Value == va.IssuerDomain {
				valid = true
				return
			} else if caa.Flag > 0 {
				valid = false
				return
			}
		}

		valid = false
		return
	}

	return
}

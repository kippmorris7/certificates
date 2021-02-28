package acme

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/smallstep/nosql"
	"go.step.sm/crypto/jose"
)

// Challenge represents an ACME response Challenge type.
type Challenge struct {
	Type      string  `json:"type"`
	Status    string  `json:"status"`
	Token     string  `json:"token"`
	Validated string  `json:"validated,omitempty"`
	URL       string  `json:"url"`
	Error     *AError `json:"error,omitempty"`
	ID        string  `json:"-"`
	AuthzID   string  `json:"-"`
	AccountID string  `json:"-"`
	Value     string  `json:"-"`
}

// ToLog enables response logging.
func (ch *Challenge) ToLog() (interface{}, error) {
	b, err := json.Marshal(ch)
	if err != nil {
		return nil, ServerInternalErr(errors.Wrap(err, "error marshaling challenge for logging"))
	}
	return string(b), nil
}

// Validate attempts to validate the challenge. Stores changes to the Challenge
// type using the DB interface.
// satisfactorily validated, the 'status' and 'validated' attributes are
// updated.
func (ch *Challenge) Validate(ctx context.Context, db DB, jwk *jose.JSONWebKey, vo validateOptions) error {
	// If already valid or invalid then return without performing validation.
	if ch.Status == StatusValid || ch.Status == StatusInvalid {
		return nil
	}
	switch ch.Type {
	case "http-01":
		return http01Validate(ctx, ch, db, jwk, vo)
	case "dns-01":
		return dns01Validate(ctx, ch, db, jwk, vo)
	case "tls-alpn-01":
		return tlsalpn01Validate(ctx, ch, db, jwk, vo)
	default:
		return ServerInternalErr(errors.Errorf("unexpected challenge type '%s'", ch.Type))
	}
}

func http01Validate(ctx context.Context, ch *Challenge, db DB, jwk *jose.JSONWebKey, vo validateOptions) error {
	url := fmt.Sprintf("http://%s/.well-known/acme-challenge/%s", ch.Value, ch.Token)

	resp, err := vo.httpGet(url)
	if err != nil {
		return storeError(ctx, ch, db, ConnectionErr(errors.Wrapf(err,
			"error doing http GET for url %s", url)))
	}
	if resp.StatusCode >= 400 {
		return storeError(ctx, ch, db, ConnectionErr(errors.Errorf("error doing http GET for url %s with status code %d",
			url, resp.StatusCode)))
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return ServerInternalErr(errors.Wrapf(err, "error reading "+
			"response body for url %s", url))
	}
	keyAuth := strings.Trim(string(body), "\r\n")

	expected, err := KeyAuthorization(ch.Token, jwk)
	if err != nil {
		return err
	}
	if keyAuth != expected {
		return storeError(ctx, ch, db, RejectedIdentifierErr(errors.Errorf("keyAuthorization does not match; "+
			"expected %s, but got %s", expected, keyAuth)))
	}

	// Update and store the challenge.
	ch.Status = StatusValid
	ch.Error = nil
	ch.Validated = clock.Now()

	return ServerInternalErr(db.UpdateChallenge(ctx, ch))
}

func tlsalpn01Validate(ctx context.Context, ch *Challenge, db DB, jwk *jose.JSONWebKey, vo validateOptions) error {
	config := &tls.Config{
		NextProtos:         []string{"acme-tls/1"},
		ServerName:         tc.Value,
		InsecureSkipVerify: true, // we expect a self-signed challenge certificate
	}

	hostPort := net.JoinHostPort(tc.Value, "443")

	conn, err := vo.tlsDial("tcp", hostPort, config)
	if err != nil {
		return storeError(ctx, ch, db, ConnectionErr(errors.Wrapf(err,
			"error doing TLS dial for %s", hostPort)))
	}
	defer conn.Close()

	cs := conn.ConnectionState()
	certs := cs.PeerCertificates

	if len(certs) == 0 {
		return storeError(ctx, ch, db, RejectedIdentifierErr(errors.Errorf("%s "+
			"challenge for %s resulted in no certificates", tc.Type, tc.Value)))
	}

	if !cs.NegotiatedProtocolIsMutual || cs.NegotiatedProtocol != "acme-tls/1" {
		return storeError(ctx, ch, db, RejectedIdentifierErr(errors.Errorf("cannot "+
			"negotiate ALPN acme-tls/1 protocol for tls-alpn-01 challenge")))
	}

	leafCert := certs[0]

	if len(leafCert.DNSNames) != 1 || !strings.EqualFold(leafCert.DNSNames[0], tc.Value) {
		return storeError(ctx, ch, db, RejectedIdentifierErr(errors.Errorf("incorrect certificate for tls-alpn-01 challenge: "+
			"leaf certificate must contain a single DNS name, %v", tc.Value)))
	}

	idPeAcmeIdentifier := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 31}
	idPeAcmeIdentifierV1Obsolete := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 30, 1}
	foundIDPeAcmeIdentifierV1Obsolete := false

	keyAuth, err := KeyAuthorization(tc.Token, jwk)
	if err != nil {
		return nil, err
	}
	hashedKeyAuth := sha256.Sum256([]byte(keyAuth))

	for _, ext := range leafCert.Extensions {
		if idPeAcmeIdentifier.Equal(ext.Id) {
			if !ext.Critical {
				return storeError(ctx, ch, db, RejectedIdentifierErr(errors.Errorf("incorrect "+
					"certificate for tls-alpn-01 challenge: acmeValidationV1 extension not critical")))
			}

			var extValue []byte
			rest, err := asn1.Unmarshal(ext.Value, &extValue)

			if err != nil || len(rest) > 0 || len(hashedKeyAuth) != len(extValue) {
				return storeError(ctx, ch, db, RejectedIdentifierErr(errors.Errorf("incorrect "+
					"certificate for tls-alpn-01 challenge: malformed acmeValidationV1 extension value")))
			}

			if subtle.ConstantTimeCompare(hashedKeyAuth[:], extValue) != 1 {
				return storeError(ctx, ch, db, RejectedIdentifierErr(errors.Errorf("incorrect certificate for tls-alpn-01 challenge: "+
					"expected acmeValidationV1 extension value %s for this challenge but got %s",
					hex.EncodeToString(hashedKeyAuth[:]), hex.EncodeToString(extValue))))
			}

			ch.Status = StatusValid
			ch.Error = nil
			ch.Validated = clock.Now()

			return ServerInternalErr(db.UpdateChallenge(ctx, ch))
		}

		if idPeAcmeIdentifierV1Obsolete.Equal(ext.Id) {
			foundIDPeAcmeIdentifierV1Obsolete = true
		}
	}

	if foundIDPeAcmeIdentifierV1Obsolete {
		return storeError(ctx, ch, db, RejectedIdentifierErr(errors.Errorf("incorrect "+
			"certificate for tls-alpn-01 challenge: obsolete id-pe-acmeIdentifier in acmeValidationV1 extension")))
	}

	return storeError(ctx, ch, db, RejectedIdentifierErr(errors.Errorf("incorrect "+
		"certificate for tls-alpn-01 challenge: missing acmeValidationV1 extension")))
}

func dns01Validate(ctx context.Context, ch *Challenge, db nosql.DB, jwk *jose.JSONWebKey, vo validateOptions) error {
	// Normalize domain for wildcard DNS names
	// This is done to avoid making TXT lookups for domains like
	// _acme-challenge.*.example.com
	// Instead perform txt lookup for _acme-challenge.example.com
	domain := strings.TrimPrefix(dc.Value, "*.")

	txtRecords, err := vo.lookupTxt("_acme-challenge." + domain)
	if err != nil {
		return storeError(ctx, ch, db, DNSErr(errors.Wrapf(err, "error looking up TXT "+
			"records for domain %s", domain)))
	}

	expectedKeyAuth, err := KeyAuthorization(dc.Token, jwk)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256([]byte(expectedKeyAuth))
	expected := base64.RawURLEncoding.EncodeToString(h[:])
	var found bool
	for _, r := range txtRecords {
		if r == expected {
			found = true
			break
		}
	}
	if !found {
		return storeError(ctx, ch, db, RejectedIdentifierErr(errors.Errorf("keyAuthorization "+
			"does not match; expected %s, but got %s", expectedKeyAuth, txtRecords)))
	}

	// Update and store the challenge.
	ch.Status = StatusValid
	ch.Error = nil
	ch.Validated = time.Now().UTC()

	return ServerInternalErr(db.UpdateChallenge(ctx, ch))
}

// KeyAuthorization creates the ACME key authorization value from a token
// and a jwk.
func KeyAuthorization(token string, jwk *jose.JSONWebKey) (string, error) {
	thumbprint, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", ServerInternalErr(errors.Wrap(err, "error generating JWK thumbprint"))
	}
	encPrint := base64.RawURLEncoding.EncodeToString(thumbprint)
	return fmt.Sprintf("%s.%s", token, encPrint), nil
}

// storeError the given error to an ACME error and saves using the DB interface.
func (bc *baseChallenge) storeError(ctx context.Context, ch Challenge, db nosql.DB, err *Error) error {
	ch.Error = err.ToACME()
	if err := db.UpdateChallenge(ctx, ch); err != nil {
		return ServerInternalErr(errors.Wrap(err, "failure saving error to acme challenge"))
	}
	return nil
}

type httpGetter func(string) (*http.Response, error)
type lookupTxt func(string) ([]string, error)
type tlsDialer func(network, addr string, config *tls.Config) (*tls.Conn, error)

type validateOptions struct {
	httpGet   httpGetter
	lookupTxt lookupTxt
	tlsDial   tlsDialer
}

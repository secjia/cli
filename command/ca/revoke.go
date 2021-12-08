package ca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/smallstep/certificates/api"
	"github.com/smallstep/certificates/authority/provisioner"
	"github.com/smallstep/certificates/ca"
	"github.com/smallstep/certificates/pki"
	"github.com/smallstep/cli/crypto/pemutil"
	"github.com/smallstep/cli/crypto/x509util"
	"github.com/smallstep/cli/flags"
	"github.com/smallstep/cli/jose"
	"github.com/smallstep/cli/ui"
	"github.com/smallstep/cli/utils/cautils"
	"github.com/urfave/cli"
	"go.step.sm/cli-utils/command"
	"go.step.sm/cli-utils/errs"
	"golang.org/x/crypto/ocsp"
)

/*
NOTE: This command currently only supports passive revocation. Passive revocation
means preventing a certificate from being renewed and letting it expire.

TODO: Add support for CRL and OCSP.
*/

func revokeCertificateCommand() cli.Command {
	return cli.Command{
		Name:   "revoke",
		Action: command.ActionFunc(revokeCertificateAction),
		Usage:  "revoke a certificate",
		UsageText: `**step ca revoke** <serial-number>
[**--cert**=<file>] [**--key**=<file>] [**--token**=<ott>]
[**--reason**=<string>] [**--reasonCode**=<code>] [**-offline**]
[**--ca-url**=<uri>] [**--root**=<file>] [**--context**=<name>]`,
		Description: `
**step ca revoke** command revokes a certificate with the given serial
number.

**Active Revocation**: A certificate is no longer valid from the moment it has
been actively revoked. Clients are required to check against centralized
sources of certificate validity information (e.g. by using CRLs (Certificate
Revocation Lists) or OCSP (Online Certificate Status Protocol)) to
verify that certificates have not been revoked. Active Revocation requires
clients to take an active role in certificate validation for the benefit of
real time revocation.

**Passive Revocation**: A certificate that has been passively revoked can no
longer be renewed. It will still be valid for the remainder of it's validity period,
but cannot be prolonged. The benefit of passive revocation is that clients
can verify certificates in a simple, decentralized manner without relying on
centralized 3rd parties. Passive revocation works best with short
certificate lifetimes.

**step ca revoke** currently only supports passive revocation. Active revocation
is on our roadmap.

A revocation request can be authorized using a JWK provisioner token, or using a
client certificate.

When you supply a serial number, you're prompted to choose a JWK provisioner,
and a provisioner token is transparently generated. Any JWK provisioner
can revoke any certificate.

When you supply a certificate and private key (with --crt and --key),
mTLS is used to authorize the revocation.

Certificates generated using the OIDC provisioner cannot be revoked
using the API token method.

## POSITIONAL ARGUMENTS

<serial-number>
:  The serial number of the certificate that should be revoked. Can be left blank,
either to be supplied by prompt, or when using the --cert and --key flags for
revocation over mTLS.

## EXAMPLES

Revoke a certificate using a transparently generated JWK provisioner token and the default
'unspecified' reason:
'''
$ step ca revoke 308893286343609293989051180431574390766
'''

Revoke a certificate using a transparently generated token and configured reason
and reasonCode:
'''
$ step ca revoke --reason "laptop compromised" --reasonCode 1 308893286343609293989051180431574390766
'''

Revoke a certificate using a transparently generated token and configured reason
and stringified reasonCode:
'''
$ step ca revoke --reason "laptop compromised" --reasonCode "key compromise" 308893286343609293989051180431574390766
'''

Revoke a certificate using that same certificate to validate and authorize the
request (rather than a token) over mTLS:
'''
$ step ca revoke --cert mike.cert --key mike.key
'''

Revoke a certificate using a JWK token, pre-generated by a provisioner, to authorize
the request with the CA:
'''
$ TOKEN=$(step ca token --revoke 308893286343609293989051180431574390766)
$ step ca revoke --token $TOKEN 308893286343609293989051180431574390766
'''

Revoke a certificate in offline mode:
'''
$ step ca revoke --offline 308893286343609293989051180431574390766
'''

Revoke a certificate in offline mode using --cert and --key (the cert/key pair
will be validated against the root and intermediate certifcates configured in
the step CA):
'''
$ step ca revoke --offline --cert foo.crt --key foo.key
'''`,
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "cert",
				Usage: `The <file> containing the cert that should be revoked.`,
			},
			cli.StringFlag{
				Name:  "key",
				Usage: `The <file> containing the key corresponding to the cert that should be revoked.`,
			},
			cli.StringFlag{
				Name:  "reason",
				Usage: `The <string> representing the reason for which the cert is being revoked.`,
			},
			cli.StringFlag{
				Name:  "reasonCode",
				Value: "",
				Usage: `The <reasonCode> specifies the reason for revocation - chose from a list of
common revocation reasons. If unset, the default is Unspecified.

: <reasonCode> can be a number from 0-9 or a case insensitive string matching
one of the following options:

    **Unspecified**
    :  No reason given (Default -- reasonCode=0).

    **KeyCompromise**
    :  The key is believed to have been compromised (reasonCode=1).

    **CACompromise**
    :  The issuing Certificate Authority itself has been compromised (reasonCode=2).

    **AffiliationChanged**
    :  The certificate contained affiliation information, for example, it may
have been an EV certificate and the associated business is no longer owned by
the same entity (reasonCode=3).

    **Superseded**
    :  The certificate is being replaced (reasonCode=4).

    **CessationOfOperation**
    :  If a CA is decommissioned, no longer to be used, the CA's certificate
should be revoked with this reason code. Do not revoke the CA's certificate if
the CA no longer issues new certificates, yet still publishes CRLs for the
currently issued certificates (reasonCode=5).

    **CertificateHold**
    :  A temporary revocation that indicates that a CA will not vouch for a
certificate at a specific point in time. Once a certificate is revoked with a
CertificateHold reason code, the certificate can then be revoked with another
Reason Code, or unrevoked and returned to use (reasonCode=6).

    **RemoveFromCRL**
    :  If a certificate is revoked with the CertificateHold reason code, it is
possible to "unrevoke" a certificate. The unrevoking process still lists the
certificate in the CRL, but with the reason code set to RemoveFromCRL.
Note: This is specific to the CertificateHold reason and is only used in DeltaCRLs
(reasonCode=8).

    **PrivilegeWithdrawn**
    :  The right to represent the given entity was revoked for some reason
(reasonCode=9).

    **AACompromise**
    :   It is known or suspected that aspects of the AA validated in the
attribute certificate have been compromised (reasonCode=10).
`,
			},
			flags.Token,
			flags.CaConfig,
			flags.Offline,
			flags.CaURL,
			flags.Root,
			flags.Context,
		},
	}
}

func revokeCertificateAction(ctx *cli.Context) error {
	args := ctx.Args()
	serial := args.Get(0)
	certFile, keyFile := ctx.String("cert"), ctx.String("key")
	token := ctx.String("token")
	offline := ctx.Bool("offline")

	// Validate the reasonCode arg early in the flow.
	if _, err := ReasonCodeToNum(ctx.String("reasonCode")); err != nil {
		return err
	}

	// offline and token are incompatible because the token is generated before
	// the start of the offline CA.
	if offline && token != "" {
		return errs.IncompatibleFlagWithFlag(ctx, "offline", "token")
	}

	// revokeFlow unifies online and offline flows on a single api.
	flow, err := newRevokeFlow(ctx, certFile, keyFile)
	if err != nil {
		return err
	}

	// If cert and key are passed then infer the serial number and certificate
	// that should be revoked.
	if len(certFile) > 0 || len(keyFile) > 0 {
		// Must be using cert/key flags for mTLS revoke so should be 0 cmd line args.
		if ctx.NArg() > 0 {
			return errors.Errorf("'%s %s --cert <certificate> --key <key>' expects no additional positional arguments", ctx.App.Name, ctx.Command.Name)
		}
		if certFile == "" {
			return errs.RequiredWithFlag(ctx, "key", "cert")
		}
		if keyFile == "" {
			return errs.RequiredWithFlag(ctx, "cert", "key")
		}
		if len(token) > 0 {
			errs.IncompatibleFlagWithFlag(ctx, "cert", "token")
		}
		if len(serial) > 0 {
			errs.IncompatibleFlagWithFlag(ctx, "cert", "serial")
		}
		var cert []*x509.Certificate
		cert, err = pemutil.ReadCertificateBundle(certFile)
		if err != nil {
			return err
		}
		serial = cert[0].SerialNumber.String()
	} else {
		// Must be using serial number so verify that only 1 command line args was given.
		if err := errs.NumberOfArguments(ctx, 1); err != nil {
			return err
		}
		if token == "" {
			// No token and no cert/key pair - so generate a token.
			token, err = flow.GenerateToken(ctx, &serial)
			if err != nil {
				return err
			}
		}
	}

	if err := flow.Revoke(ctx, serial, token); err != nil {
		return err
	}

	ui.Printf("Certificate with Serial Number %s has been revoked.\n", serial)
	return nil
}

type revokeTokenClaims struct {
	SHA string `json:"sha"`
	jose.Claims
}

type revokeFlow struct {
	offlineCA *cautils.OfflineCA
	offline   bool
}

func newRevokeFlow(ctx *cli.Context, certFile, keyFile string) (*revokeFlow, error) {
	var err error
	var offlineClient *cautils.OfflineCA

	offline := ctx.Bool("offline")
	if offline {
		caConfig := ctx.String("ca-config")
		if caConfig == "" {
			return nil, errs.InvalidFlagValue(ctx, "ca-config", "", "")
		}
		offlineClient, err = cautils.NewOfflineCA(ctx, caConfig)
		if err != nil {
			return nil, err
		}
		if len(certFile) > 0 || len(keyFile) > 0 {
			if err := offlineClient.VerifyClientCert(certFile, keyFile); err != nil {
				return nil, err
			}
		}
	}

	return &revokeFlow{
		offlineCA: offlineClient,
		offline:   offline,
	}, nil
}

func (f *revokeFlow) getClient(ctx *cli.Context, serial, token string) (cautils.CaClient, error) {
	if f.offline {
		return f.offlineCA, nil
	}

	// Create online client
	caURL, err := flags.ParseCaURLIfExists(ctx)
	if err != nil {
		return nil, err
	}
	rootFile := ctx.String("root")
	var options []ca.ClientOption

	if len(token) > 0 {
		tok, err := jose.ParseSigned(token)
		if err != nil {
			return nil, errors.Wrap(err, "error parsing flag '--token'")
		}
		var claims revokeTokenClaims
		if err := tok.UnsafeClaimsWithoutVerification(&claims); err != nil {
			return nil, errors.Wrap(err, "error parsing flag '--token'")
		}
		if !strings.EqualFold(claims.Subject, serial) {
			return nil, errors.Errorf("token subject '%s' and serial number '%s' do not match", claims.Subject, serial)
		}

		// Prepare client for bootstrap or provisioning tokens
		if len(claims.SHA) > 0 && len(claims.Audience) > 0 && strings.HasPrefix(strings.ToLower(claims.Audience[0]), "http") {
			if caURL == "" {
				caURL = claims.Audience[0]
			}
			options = append(options, ca.WithRootSHA256(claims.SHA))
			ui.PrintSelected("CA", caURL)
			return ca.NewClient(caURL, options...)
		}
	} else if caURL == "" {
		// If there is no token then caURL is required.
		return nil, errs.RequiredFlag(ctx, "ca-url")
	}

	if rootFile == "" {
		rootFile = pki.GetRootCAPath()
		if _, err := os.Stat(rootFile); err != nil {
			return nil, errs.RequiredFlag(ctx, "root")
		}
	}
	options = append(options, ca.WithRootFile(rootFile))

	ui.PrintSelected("CA", caURL)
	return ca.NewClient(caURL, options...)
}

func (f *revokeFlow) GenerateToken(ctx *cli.Context, subject *string) (string, error) {
	// For offline just generate the token
	if f.offline {
		return f.offlineCA.GenerateToken(ctx, cautils.RevokeType, *subject, nil, time.Time{}, time.Time{}, provisioner.TimeDuration{}, provisioner.TimeDuration{})
	}

	// Use online CA to get the provisioners and generate the token
	caURL, err := flags.ParseCaURLIfExists(ctx)
	if err != nil {
		return "", err
	} else if caURL == "" {
		return "", errs.RequiredUnlessFlag(ctx, "ca-url", "token")
	}

	root := ctx.String("root")
	if root == "" {
		root = pki.GetRootCAPath()
		if _, err := os.Stat(root); err != nil {
			return "", errs.RequiredUnlessFlag(ctx, "root", "token")
		}
	}

	if *subject == "" {
		*subject, err = ui.Prompt("What is the Serial Number of the certificate you would like to revoke? (`step certificate inspect foo.cert`)", ui.WithValidateNotEmpty())
		if err != nil {
			return "", err
		}
	}

	return cautils.NewTokenFlow(ctx, cautils.RevokeType, *subject, nil, caURL, root, time.Time{}, time.Time{}, provisioner.TimeDuration{}, provisioner.TimeDuration{})
}

func (f *revokeFlow) Revoke(ctx *cli.Context, serial, token string) error {
	client, err := f.getClient(ctx, serial, token)
	if err != nil {
		return err
	}

	reason := ctx.String("reason")
	// Convert the reasonCode flag to an OCSP revocation code.
	reasonCode, err := ReasonCodeToNum(ctx.String("reasonCode"))
	if err != nil {
		return err
	}

	var tr http.RoundTripper

	// If token is not provided then set up mTLS client with expected cert and key.
	if token == "" {
		certFile, keyFile := ctx.String("cert"), ctx.String("key")

		certPEMBytes, err := os.ReadFile(certFile)
		if err != nil {
			return errors.Wrap(err, "error reading certificate")
		}
		key, err := pemutil.Read(keyFile)
		if err != nil {
			return errors.Wrap(err, "error parsing key")
		}
		keyBlock, err := pemutil.Serialize(key)
		if err != nil {
			return errors.Wrap(err, "error serializing key")
		}

		cert, err := tls.X509KeyPair(certPEMBytes, pem.EncodeToMemory(keyBlock))
		if err != nil {
			return errors.Wrap(err, "error loading certificate key pair")
		}
		if len(cert.Certificate) == 0 {
			return errors.New("error loading certificate: certificate chain is empty")
		}
		root := ctx.String("root")
		if root == "" {
			root = pki.GetRootCAPath()
			if _, err = os.Stat(root); err != nil {
				return errs.RequiredUnlessFlag(ctx, "root", "token")
			}
		}
		var rootCAs *x509.CertPool
		rootCAs, err = x509util.ReadCertPool(root)
		if err != nil {
			return err
		}
		tr = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				RootCAs:                  rootCAs,
				PreferServerCipherSuites: true,
				Certificates:             []tls.Certificate{cert},
			},
		}
	}

	req := &api.RevokeRequest{
		Serial:     serial,
		Reason:     reason,
		ReasonCode: reasonCode,
		OTT:        token,
		Passive:    true,
	}
	if _, err = client.Revoke(req, tr); err != nil {
		return err
	}
	return nil
}

// RevocationReasonCodes is a map between string reason codes
// to integers as defined in RFC 5280
var RevocationReasonCodes = map[string]int{
	"unspecified":          ocsp.Unspecified,
	"keycompromise":        ocsp.KeyCompromise,
	"cacompromise":         ocsp.CACompromise,
	"affiliationchanged":   ocsp.AffiliationChanged,
	"superseded":           ocsp.Superseded,
	"cessationofoperation": ocsp.CessationOfOperation,
	"certificatehold":      ocsp.CertificateHold,
	"removefromcrl":        ocsp.RemoveFromCRL,
	"privilegewithdrawn":   ocsp.PrivilegeWithdrawn,
	"aacompromise":         ocsp.AACompromise,
}

// ReasonCodeToNum converts a string encoded code to a number.
// 1) "4" -> 4
// 2) "key compromise" -> 1
// 3) "keYComPromIse" -> 1
func ReasonCodeToNum(rc string) (int, error) {
	// default to 0
	if rc == "" {
		return 0, nil
	}

	if code, err := strconv.Atoi(rc); err == nil {
		if code < ocsp.Unspecified || code > ocsp.AACompromise {
			return -1, errors.Errorf("reasonCode out of bounds. Got %d, but want value between %d and %d",
				code, ocsp.Unspecified, ocsp.AACompromise)
		}
		return code, nil
	}

	code, found := RevocationReasonCodes[strings.ToLower(strings.ReplaceAll(rc, " ", ""))]
	if !found {
		return 0, errors.Errorf("unrecognized revocation reason code '%s'", rc)
	}

	return code, nil
}

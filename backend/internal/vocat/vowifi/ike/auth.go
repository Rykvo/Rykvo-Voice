package ike

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"rykvo.local/auth/internal/vocat/vowifi"
)

const (
	authMethodRSASignature     = 1
	authMethodSharedKeyMIC     = 2
	authMethodECDSASHA256P256  = 9
	authMethodECDSASHA384P384  = 10
	authMethodECDSASHA512P521  = 11
	authMethodDigitalSignature = 14
)

var (
	oidSHA256WithRSA   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidSHA384WithRSA   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 12}
	oidSHA512WithRSA   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 13}
	oidECDSAWithSHA256 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidECDSAWithSHA384 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 3}
	oidECDSAWithSHA512 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 4}
)

func responderSignedOctets(
	initialResponse []byte,
	initiatorNonce []byte,
	suite negotiatedSuite,
	skpr []byte,
	idr payload,
) ([]byte, error) {
	idHash, err := prf(suite, skpr, idr.Body)
	if err != nil {
		return nil, err
	}
	signed := make([]byte, 0, len(initialResponse)+len(initiatorNonce)+len(idHash))
	signed = append(signed, initialResponse...)
	signed = append(signed, initiatorNonce...)
	signed = append(signed, idHash...)
	return signed, nil
}

func initiatorSignedOctets(
	initialRequest []byte,
	responderNonce []byte,
	suite negotiatedSuite,
	skpi []byte,
	idi payload,
) ([]byte, error) {
	idHash, err := prf(suite, skpi, idi.Body)
	if err != nil {
		return nil, err
	}
	signed := make([]byte, 0, len(initialRequest)+len(responderNonce)+len(idHash))
	signed = append(signed, initialRequest...)
	signed = append(signed, responderNonce...)
	signed = append(signed, idHash...)
	return signed, nil
}

func makeEAPInitiatorAUTH(
	msk []byte,
	initialRequest []byte,
	responderNonce []byte,
	suite negotiatedSuite,
	skpi []byte,
	idi payload,
) (payload, error) {
	signed, err := initiatorSignedOctets(initialRequest, responderNonce, suite, skpi, idi)
	if err != nil {
		return payload{}, err
	}
	paddedKey, err := prf(suite, msk, []byte("Key Pad for IKEv2"))
	if err != nil {
		return payload{}, err
	}
	authValue, err := prf(suite, paddedKey, signed)
	if err != nil {
		return payload{}, err
	}
	body := make([]byte, 4+len(authValue))
	body[0] = authMethodSharedKeyMIC
	copy(body[4:], authValue)
	return payload{Type: payloadAuth, Body: body}, nil
}

func verifyEAPResponderAUTH(
	auth payload,
	msk []byte,
	initialResponse []byte,
	initiatorNonce []byte,
	suite negotiatedSuite,
	skpr []byte,
	idr payload,
) error {
	if len(auth.Body) < 4 || auth.Body[0] != authMethodSharedKeyMIC {
		return errors.New("ike: final responder AUTH does not use the EAP shared-key MIC")
	}
	signed, err := responderSignedOctets(initialResponse, initiatorNonce, suite, skpr, idr)
	if err != nil {
		return err
	}
	paddedKey, err := prf(suite, msk, []byte("Key Pad for IKEv2"))
	if err != nil {
		return err
	}
	expected, err := prf(suite, paddedKey, signed)
	if err != nil {
		return err
	}
	if len(auth.Body[4:]) != len(expected) || subtle.ConstantTimeCompare(auth.Body[4:], expected) != 1 {
		return errors.New("ike: final responder AUTH is invalid")
	}
	return nil
}

func validateInitialResponderAUTH(
	payloads []payload,
	initialResponse []byte,
	initiatorNonce []byte,
	suite negotiatedSuite,
	skpr []byte,
	expectedIDr string,
	serverName string,
	roots *x509.CertPool,
	pinned crypto.PublicKey,
	allowMissing bool,
) (vowifi.ResponderAUTHStatus, payload, error) {
	idrPayloads := payloadsOfType(payloads, payloadIDr)
	authPayloads := payloadsOfType(payloads, payloadAuth)
	if len(authPayloads) == 0 {
		if len(idrPayloads) > 1 {
			return vowifi.ResponderAUTHInvalid, payload{}, errors.New("ike: duplicate responder identity payload")
		}
		if allowMissing {
			if len(idrPayloads) == 1 {
				return vowifi.ResponderAUTHMissing, idrPayloads[0], nil
			}
			return vowifi.ResponderAUTHMissing, payload{}, nil
		}
		return vowifi.ResponderAUTHMissing, payload{}, vowifi.ErrResponderAUTHRequired
	}
	if len(authPayloads) != 1 || len(idrPayloads) != 1 {
		return vowifi.ResponderAUTHInvalid, payload{}, errors.New("ike: responder AUTH requires exactly one IDr and AUTH payload")
	}
	idr := idrPayloads[0]
	auth := authPayloads[0]
	if len(idr.Body) < 4 || len(auth.Body) < 5 {
		return vowifi.ResponderAUTHInvalid, idr, errors.New("ike: responder IDr or AUTH payload is truncated")
	}
	if err := validateFQDNIDr(idr, expectedIDr, "initial ePDG"); err != nil {
		return vowifi.ResponderAUTHInvalid, idr, err
	}
	publicKey := pinned
	if publicKey == nil {
		certificates, err := parseResponderCertificates(payloads)
		if err != nil {
			return vowifi.ResponderAUTHInvalid, idr, err
		}
		if len(certificates) == 0 {
			return vowifi.ResponderAUTHInvalid, idr, errors.New("ike: responder AUTH has no certificate or pinned public key")
		}
		if err := verifyResponderCertificate(certificates, roots, serverName); err != nil {
			return vowifi.ResponderAUTHInvalid, idr, err
		}
		publicKey = certificates[0].PublicKey
	}
	signed, err := responderSignedOctets(initialResponse, initiatorNonce, suite, skpr, idr)
	if err != nil {
		return vowifi.ResponderAUTHInvalid, idr, err
	}
	if err := verifyDigitalAUTH(publicKey, auth.Body[0], auth.Body[4:], signed); err != nil {
		return vowifi.ResponderAUTHInvalid, idr, fmt.Errorf("ike: invalid responder AUTH: %w", err)
	}
	return vowifi.ResponderAUTHVerified, idr, nil
}

func validateFQDNIDr(idr payload, expectedIDr string, label string) error {
	if len(idr.Body) < 4 {
		return errors.New("ike: responder identity is truncated")
	}
	identityType := idr.Body[0]
	identity := strings.TrimSpace(string(idr.Body[4:]))
	if identityType != 2 {
		return fmt.Errorf("ike: %s IDr must use ID_FQDN, got type %d", label, identityType)
	}
	if identity == "" {
		return fmt.Errorf("ike: %s IDr is empty", label)
	}
	if expectedIDr != "" && !strings.EqualFold(strings.TrimSuffix(identity, "."), strings.TrimSuffix(expectedIDr, ".")) {
		return fmt.Errorf("ike: %s IDr %q does not match %q", label, identity, expectedIDr)
	}
	return nil
}

func parseResponderCertificates(payloads []payload) ([]*x509.Certificate, error) {
	var certificates []*x509.Certificate
	for _, item := range payloadsOfType(payloads, payloadCert) {
		if len(item.Body) < 2 {
			return nil, errors.New("ike: responder certificate payload is truncated")
		}
		if item.Body[0] != 4 {
			return nil, fmt.Errorf("ike: unsupported responder certificate encoding %d", item.Body[0])
		}
		certificate, err := x509.ParseCertificate(item.Body[1:])
		if err != nil {
			return nil, fmt.Errorf("ike: parse responder certificate: %w", err)
		}
		certificates = append(certificates, certificate)
	}
	return certificates, nil
}

func verifyResponderCertificate(certificates []*x509.Certificate, roots *x509.CertPool, serverName string) error {
	if len(certificates) == 0 {
		return errors.New("ike: no responder certificate")
	}
	if roots == nil {
		var err error
		roots, err = x509.SystemCertPool()
		if err != nil {
			return fmt.Errorf("ike: load system certificate roots: %w", err)
		}
	}
	intermediates := x509.NewCertPool()
	for _, certificate := range certificates[1:] {
		intermediates.AddCert(certificate)
	}
	options := x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSName:       strings.TrimSuffix(serverName, "."),
	}
	if _, err := certificates[0].Verify(options); err != nil {
		return fmt.Errorf("ike: verify responder certificate: %w", err)
	}
	return nil
}

func verifyDigitalAUTH(publicKey crypto.PublicKey, method uint8, signature, signed []byte) error {
	switch method {
	case authMethodRSASignature:
		key, ok := publicKey.(*rsa.PublicKey)
		if !ok {
			return errors.New("RSA AUTH used with a non-RSA public key")
		}
		digest := sha1.Sum(signed)
		return rsa.VerifyPKCS1v15(key, crypto.SHA1, digest[:], signature)
	case authMethodECDSASHA256P256:
		return verifyRawECDSA(publicKey, crypto.SHA256, signature, signed)
	case authMethodECDSASHA384P384:
		return verifyRawECDSA(publicKey, crypto.SHA384, signature, signed)
	case authMethodECDSASHA512P521:
		return verifyRawECDSA(publicKey, crypto.SHA512, signature, signed)
	case authMethodDigitalSignature:
		return verifyGenericSignature(publicKey, signature, signed)
	default:
		return fmt.Errorf("unsupported responder AUTH method %d", method)
	}
}

func verifyRawECDSA(publicKey crypto.PublicKey, algorithm crypto.Hash, signature, signed []byte) error {
	key, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("ECDSA AUTH used with a non-ECDSA public key")
	}
	size := (key.Curve.Params().BitSize + 7) / 8
	if len(signature) != size*2 {
		return fmt.Errorf("ECDSA signature length %d does not match curve size %d", len(signature), size)
	}
	digest, err := hashSignedOctets(algorithm, signed)
	if err != nil {
		return err
	}
	r := new(big.Int).SetBytes(signature[:size])
	s := new(big.Int).SetBytes(signature[size:])
	if !ecdsa.Verify(key, digest, r, s) {
		return errors.New("ECDSA signature verification failed")
	}
	return nil
}

func verifyGenericSignature(publicKey crypto.PublicKey, encoded, signed []byte) error {
	var algorithm pkix.AlgorithmIdentifier
	rest, err := asn1.Unmarshal(encoded, &algorithm)
	if err != nil || len(rest) == 0 {
		return errors.New("generic digital signature has an invalid AlgorithmIdentifier")
	}
	var hashAlgorithm crypto.Hash
	var isRSA bool
	switch {
	case algorithm.Algorithm.Equal(oidSHA256WithRSA):
		hashAlgorithm, isRSA = crypto.SHA256, true
	case algorithm.Algorithm.Equal(oidSHA384WithRSA):
		hashAlgorithm, isRSA = crypto.SHA384, true
	case algorithm.Algorithm.Equal(oidSHA512WithRSA):
		hashAlgorithm, isRSA = crypto.SHA512, true
	case algorithm.Algorithm.Equal(oidECDSAWithSHA256):
		hashAlgorithm = crypto.SHA256
	case algorithm.Algorithm.Equal(oidECDSAWithSHA384):
		hashAlgorithm = crypto.SHA384
	case algorithm.Algorithm.Equal(oidECDSAWithSHA512):
		hashAlgorithm = crypto.SHA512
	default:
		return fmt.Errorf("unsupported generic signature algorithm %s", algorithm.Algorithm.String())
	}
	digest, err := hashSignedOctets(hashAlgorithm, signed)
	if err != nil {
		return err
	}
	if isRSA {
		key, ok := publicKey.(*rsa.PublicKey)
		if !ok {
			return errors.New("RSA signature used with a non-RSA public key")
		}
		return rsa.VerifyPKCS1v15(key, hashAlgorithm, digest, rest)
	}
	key, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("ECDSA signature used with a non-ECDSA public key")
	}
	if !ecdsa.VerifyASN1(key, digest, rest) {
		return errors.New("ECDSA generic signature verification failed")
	}
	return nil
}

func hashSignedOctets(algorithm crypto.Hash, signed []byte) ([]byte, error) {
	switch algorithm {
	case crypto.SHA1:
		sum := sha1.Sum(signed)
		return sum[:], nil
	case crypto.SHA256:
		sum := sha256.Sum256(signed)
		return sum[:], nil
	case crypto.SHA384:
		sum := sha512.Sum384(signed)
		return sum[:], nil
	case crypto.SHA512:
		sum := sha512.Sum512(signed)
		return sum[:], nil
	default:
		return nil, fmt.Errorf("unsupported signature hash %v", algorithm)
	}
}

func equalPublicKeys(first, second crypto.PublicKey) bool {
	firstDER, firstErr := x509.MarshalPKIXPublicKey(first)
	secondDER, secondErr := x509.MarshalPKIXPublicKey(second)
	return firstErr == nil && secondErr == nil && bytes.Equal(firstDER, secondDER)
}

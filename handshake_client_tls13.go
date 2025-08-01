// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tls

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/mlkem"
	"crypto/rsa"
	"crypto/subtle"
	"errors"
	"fmt"
	"hash"
	"slices"
	"time"

	"github.com/excitedplus1s/utlscm/internal/hkdf"
	"github.com/excitedplus1s/utlscm/internal/tls13"
)

type clientHandshakeStateTLS13 struct {
	c            *Conn
	ctx          context.Context
	serverHello  *serverHelloMsg
	hello        *clientHelloMsg
	keyShareKeys *keySharePrivateKeys

	session     *SessionState
	earlySecret *tls13.EarlySecret
	binderKey   []byte

	certReq       *certificateRequestMsgTLS13
	usingPSK      bool
	sentDummyCCS  bool
	suite         *cipherSuiteTLS13
	transcript    hash.Hash
	masterSecret  *tls13.MasterSecret
	trafficSecret []byte // client_application_traffic_secret_0

	echContext *echClientContext

	uconn *UConn // [uTLS]
}

// handshake requires hs.c, hs.hello, hs.serverHello, hs.keyShareKeys, and,
// optionally, hs.session, hs.earlySecret and hs.binderKey to be set.
func (hs *clientHandshakeStateTLS13) handshake() error {
	c := hs.c

	// The server must not select TLS 1.3 in a renegotiation. See RFC 8446,
	// sections 4.1.2 and 4.1.3.
	if c.handshakes > 0 {
		c.sendAlert(alertProtocolVersion)
		return errors.New("tls: server selected TLS 1.3 in a renegotiation")
	}

	// Consistency check on the presence of a keyShare and its parameters.
	if hs.keyShareKeys == nil || hs.keyShareKeys.ecdhe == nil || len(hs.hello.keyShares) == 0 {
		return c.sendAlert(alertInternalError)
	}

	if err := hs.checkServerHelloOrHRR(); err != nil {
		return err
	}

	hs.transcript = hs.suite.hash.New()

	if err := transcriptMsg(hs.hello, hs.transcript); err != nil {
		return err
	}

	if hs.echContext != nil {
		hs.echContext.innerTranscript = hs.suite.hash.New()
		// [uTLS SECTION BEGIN]
		if hs.uconn != nil && hs.uconn.clientHelloBuildStatus == BuildByUtls {
			if err := hs.uconn.echTranscriptMsg(hs.hello, hs.echContext); err != nil {
				return err
			}
		} else {
			if err := transcriptMsg(hs.echContext.innerHello, hs.echContext.innerTranscript); err != nil {
				return err
			}
		}
		// [uTLS SECTION END]
	}

	if bytes.Equal(hs.serverHello.random, helloRetryRequestRandom) {
		if err := hs.sendDummyChangeCipherSpec(); err != nil {
			return err
		}
		if err := hs.processHelloRetryRequest(); err != nil {
			return err
		}
	}

	if hs.echContext != nil {
		confTranscript := cloneHash(hs.echContext.innerTranscript, hs.suite.hash)
		confTranscript.Write(hs.serverHello.original[:30])
		confTranscript.Write(make([]byte, 8))
		confTranscript.Write(hs.serverHello.original[38:])
		acceptConfirmation := tls13.ExpandLabel(hs.suite.hash.New,
			hkdf.Extract(hs.suite.hash.New, hs.echContext.innerHello.random, nil),
			"ech accept confirmation",
			confTranscript.Sum(nil),
			8,
		)
		if subtle.ConstantTimeCompare(acceptConfirmation, hs.serverHello.random[len(hs.serverHello.random)-8:]) == 1 {
			hs.hello = hs.echContext.innerHello
			c.serverName = c.config.ServerName
			hs.transcript = hs.echContext.innerTranscript
			c.echAccepted = true

			if hs.serverHello.encryptedClientHello != nil {
				c.sendAlert(alertUnsupportedExtension)
				return errors.New("tls: unexpected encrypted client hello extension in server hello despite ECH being accepted")
			}

			if hs.hello.serverName == "" && hs.serverHello.serverNameAck {
				c.sendAlert(alertUnsupportedExtension)
				return errors.New("tls: unexpected server_name extension in server hello")
			}
		} else {
			hs.echContext.echRejected = true
		}
	}

	if err := transcriptMsg(hs.serverHello, hs.transcript); err != nil {
		return err
	}

	c.buffering = true
	if err := hs.processServerHello(); err != nil {
		return err
	}
	if err := hs.sendDummyChangeCipherSpec(); err != nil {
		return err
	}
	if err := hs.establishHandshakeKeys(); err != nil {
		return err
	}
	if err := hs.readServerParameters(); err != nil {
		return err
	}
	if err := hs.readServerCertificate(); err != nil {
		return err
	}
	if err := hs.readServerFinished(); err != nil {
		return err
	}
	// [UTLS SECTION START]
	if err := hs.serverFinishedReceived(); err != nil {
		return err
	}
	// [UTLS SECTION END]
	if err := hs.sendClientCertificate(); err != nil {
		return err
	}
	if err := hs.sendClientFinished(); err != nil {
		return err
	}
	if _, err := c.flush(); err != nil {
		return err
	}

	if hs.echContext != nil && hs.echContext.echRejected {
		c.sendAlert(alertECHRequired)
		return &ECHRejectionError{hs.echContext.retryConfigs}
	}

	c.isHandshakeComplete.Store(true)

	return nil
}

// checkServerHelloOrHRR does validity checks that apply to both ServerHello and
// HelloRetryRequest messages. It sets hs.suite.
func (hs *clientHandshakeStateTLS13) checkServerHelloOrHRR() error {
	c := hs.c

	if hs.serverHello.supportedVersion == 0 {
		c.sendAlert(alertMissingExtension)
		return errors.New("tls: server selected TLS 1.3 using the legacy version field")
	}

	if hs.serverHello.supportedVersion != VersionTLS13 {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server selected an invalid version after a HelloRetryRequest")
	}

	if hs.serverHello.vers != VersionTLS12 {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server sent an incorrect legacy version")
	}

	if hs.serverHello.ocspStapling ||
		hs.serverHello.ticketSupported ||
		hs.serverHello.extendedMasterSecret ||
		hs.serverHello.secureRenegotiationSupported ||
		len(hs.serverHello.secureRenegotiation) != 0 ||
		len(hs.serverHello.alpnProtocol) != 0 ||
		len(hs.serverHello.scts) != 0 {
		c.sendAlert(alertUnsupportedExtension)
		return errors.New("tls: server sent a ServerHello extension forbidden in TLS 1.3")
	}

	if !bytes.Equal(hs.hello.sessionId, hs.serverHello.sessionId) {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server did not echo the legacy session ID")
	}

	if hs.serverHello.compressionMethod != compressionNone {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server selected unsupported compression format")
	}

	selectedSuite := mutualCipherSuiteTLS13(hs.hello.cipherSuites, hs.serverHello.cipherSuite)
	if hs.suite != nil && selectedSuite != hs.suite {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server changed cipher suite after a HelloRetryRequest")
	}
	if selectedSuite == nil {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server chose an unconfigured cipher suite")
	}
	hs.suite = selectedSuite
	c.cipherSuite = hs.suite.id

	return nil
}

// sendDummyChangeCipherSpec sends a ChangeCipherSpec record for compatibility
// with middleboxes that didn't implement TLS correctly. See RFC 8446, Appendix D.4.
func (hs *clientHandshakeStateTLS13) sendDummyChangeCipherSpec() error {
	if hs.c.quic != nil {
		return nil
	}
	if hs.sentDummyCCS {
		return nil
	}
	hs.sentDummyCCS = true

	return hs.c.writeChangeCipherRecord()
}

// processHelloRetryRequest handles the HRR in hs.serverHello, modifies and
// resends hs.hello, and reads the new ServerHello into hs.serverHello.
func (hs *clientHandshakeStateTLS13) processHelloRetryRequest() error {
	c := hs.c

	// The first ClientHello gets double-hashed into the transcript upon a
	// HelloRetryRequest. (The idea is that the server might offload transcript
	// storage to the client in the cookie.) See RFC 8446, Section 4.4.1.
	chHash := hs.transcript.Sum(nil)
	hs.transcript.Reset()
	hs.transcript.Write([]byte{typeMessageHash, 0, 0, uint8(len(chHash))})
	hs.transcript.Write(chHash)
	if err := transcriptMsg(hs.serverHello, hs.transcript); err != nil {
		return err
	}

	var isInnerHello bool
	hello := hs.hello
	if hs.echContext != nil {
		chHash = hs.echContext.innerTranscript.Sum(nil)
		hs.echContext.innerTranscript.Reset()
		hs.echContext.innerTranscript.Write([]byte{typeMessageHash, 0, 0, uint8(len(chHash))})
		hs.echContext.innerTranscript.Write(chHash)

		if hs.serverHello.encryptedClientHello != nil {
			if len(hs.serverHello.encryptedClientHello) != 8 {
				hs.c.sendAlert(alertDecodeError)
				return errors.New("tls: malformed encrypted client hello extension")
			}

			confTranscript := cloneHash(hs.echContext.innerTranscript, hs.suite.hash)
			hrrHello := make([]byte, len(hs.serverHello.original))
			copy(hrrHello, hs.serverHello.original)
			hrrHello = bytes.Replace(hrrHello, hs.serverHello.encryptedClientHello, make([]byte, 8), 1)
			confTranscript.Write(hrrHello)
			acceptConfirmation := tls13.ExpandLabel(hs.suite.hash.New,
				hkdf.Extract(hs.suite.hash.New, hs.echContext.innerHello.random, nil),
				"hrr ech accept confirmation",
				confTranscript.Sum(nil),
				8,
			)
			if subtle.ConstantTimeCompare(acceptConfirmation, hs.serverHello.encryptedClientHello) == 1 {
				hello = hs.echContext.innerHello
				c.serverName = c.config.ServerName
				isInnerHello = true
				c.echAccepted = true
			}
		}

		if err := transcriptMsg(hs.serverHello, hs.echContext.innerTranscript); err != nil {
			return err
		}
	} else if hs.serverHello.encryptedClientHello != nil {
		// Unsolicited ECH extension should be rejected
		c.sendAlert(alertUnsupportedExtension)
		return errors.New("tls: unexpected encrypted client hello extension in serverHello")
	}

	// The only HelloRetryRequest extensions we support are key_share and
	// cookie, and clients must abort the handshake if the HRR would not result
	// in any change in the ClientHello.
	if hs.serverHello.selectedGroup == 0 && hs.serverHello.cookie == nil {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server sent an unnecessary HelloRetryRequest message")
	}

	if hs.serverHello.cookie != nil {
		hello.cookie = hs.serverHello.cookie
	}

	if hs.serverHello.serverShare.group != 0 {
		c.sendAlert(alertDecodeError)
		return errors.New("tls: received malformed key_share extension")
	}

	// If the server sent a key_share extension selecting a group, ensure it's
	// a group we advertised but did not send a key share for, and send a key
	// share for it this time.
	if curveID := hs.serverHello.selectedGroup; curveID != 0 {
		if !slices.Contains(hello.supportedCurves, curveID) {
			c.sendAlert(alertIllegalParameter)
			return errors.New("tls: server selected unsupported group")
		}
		if slices.ContainsFunc(hs.hello.keyShares, func(ks keyShare) bool {
			return ks.group == curveID
		}) {
			c.sendAlert(alertIllegalParameter)
			return errors.New("tls: server sent an unnecessary HelloRetryRequest key_share")
		}
		// Note: we don't support selecting X25519MLKEM768 in a HRR, because it
		// is currently first in preference order, so if it's enabled we'll
		// always send a key share for it.
		//
		// This will have to change once we support multiple hybrid KEMs.
		if _, ok := curveForCurveID(curveID); !ok {
			c.sendAlert(alertInternalError)
			return errors.New("tls: CurvePreferences includes unsupported curve")
		}
		key, err := generateECDHEKey(c.config.rand(), curveID)
		if err != nil {
			c.sendAlert(alertInternalError)
			return err
		}
		hs.keyShareKeys = &keySharePrivateKeys{curveID: curveID, ecdhe: key}
		hello.keyShares = []keyShare{{group: curveID, data: key.PublicKey().Bytes()}}
	}

	if len(hello.pskIdentities) > 0 {
		pskSuite := cipherSuiteTLS13ByID(hs.session.cipherSuite)
		if pskSuite == nil {
			return c.sendAlert(alertInternalError)
		}
		if pskSuite.hash == hs.suite.hash {
			// Update binders and obfuscated_ticket_age.
			ticketAge := c.config.time().Sub(time.Unix(int64(hs.session.createdAt), 0))
			hello.pskIdentities[0].obfuscatedTicketAge = uint32(ticketAge/time.Millisecond) + hs.session.ageAdd

			transcript := hs.suite.hash.New()
			transcript.Write([]byte{typeMessageHash, 0, 0, uint8(len(chHash))})
			transcript.Write(chHash)
			if err := transcriptMsg(hs.serverHello, transcript); err != nil {
				return err
			}

			if err := computeAndUpdatePSK(hello, hs.binderKey, transcript, hs.suite.finishedHash); err != nil {
				return err
			}
		} else {
			// Server selected a cipher suite incompatible with the PSK.
			hello.pskIdentities = nil
			hello.pskBinders = nil
		}
	}

	// [uTLS SECTION BEGINS]
	// crypto/tls code above this point had changed crypto/tls structures in accordance with HRR, and is about
	// to call default marshaller.
	// Instead, we fill uTLS-specific structs and call uTLS marshaller.
	// Only extensionCookie, extensionPreSharedKey, extensionKeyShare, extensionEarlyData, extensionSupportedVersions,
	// and utlsExtensionPadding are supposed to change
	if hs.uconn != nil {
		if hs.uconn.ClientHelloID != HelloGolang {
			if len(hs.hello.pskIdentities) > 0 {
				// TODO: wait for someone who cares about PSK to implement
				return errors.New("uTLS does not support reprocessing of PSK key triggered by HelloRetryRequest")
			}

			keyShareExtFound := false
			for _, ext := range hs.uconn.Extensions {
				// new ks seems to be generated either way
				if ks, ok := ext.(*KeyShareExtension); ok {
					ks.KeyShares = keyShares(hs.hello.keyShares).ToPublic()
					keyShareExtFound = true
				}
			}
			if !keyShareExtFound {
				return errors.New("uTLS: received HelloRetryRequest, but keyshare not found among client's " +
					"uconn.Extensions")
			}

			if len(hs.serverHello.cookie) > 0 {
				// serverHello specified a cookie, let's echo it
				cookieFound := false
				for _, ext := range hs.uconn.Extensions {
					if ks, ok := ext.(*CookieExtension); ok {
						ks.Cookie = hs.serverHello.cookie
						cookieFound = true
					}
				}

				if !cookieFound {
					// pick a random index where to add cookieExtension
					// -2 instead of -1 is a lazy way to ensure that PSK is still a last extension
					p, err := newPRNG()
					if err != nil {
						return err
					}
					cookieIndex := p.Intn(len(hs.uconn.Extensions) - 2)
					if cookieIndex >= len(hs.uconn.Extensions) {
						// this check is for empty hs.uconn.Extensions
						return fmt.Errorf("cookieIndex >= len(hs.uconn.Extensions): %v >= %v",
							cookieIndex, len(hs.uconn.Extensions))
					}
					hs.uconn.Extensions = append(hs.uconn.Extensions[:cookieIndex],
						append([]TLSExtension{&CookieExtension{Cookie: hs.serverHello.cookie}},
							hs.uconn.Extensions[cookieIndex:]...)...)
				}
			}
			if err := hs.uconn.MarshalClientHelloNoECH(); err != nil {
				return err
			}
			hs.hello.original = hs.uconn.HandshakeState.Hello.Raw
		}
	}
	// [uTLS SECTION ENDS]
	if hello.earlyData {
		hello.earlyData = false
		c.quicRejectedEarlyData()
	}

	if isInnerHello {
		// Any extensions which have changed in hello, but are mirrored in the
		// outer hello and compressed, need to be copied to the outer hello, so
		// they can be properly decompressed by the server. For now, the only
		// extension which may have changed is keyShares.
		hs.hello.keyShares = hello.keyShares
		hs.echContext.innerHello = hello
		if hs.uconn != nil && hs.uconn.clientHelloBuildStatus == BuildByUtls {
			if err := hs.uconn.computeAndUpdateOuterECHExtension(hs.echContext.innerHello, hs.echContext, false); err != nil {
				return err
			}

			hs.hello.original = hs.uconn.HandshakeState.Hello.Raw

			if err := hs.uconn.echTranscriptMsg(hs.hello, hs.echContext); err != nil {
				return err
			}

		} else {
			if err := transcriptMsg(hs.echContext.innerHello, hs.echContext.innerTranscript); err != nil {
				return err
			}

			if err := computeAndUpdateOuterECHExtension(hs.hello, hs.echContext.innerHello, hs.echContext, false); err != nil {
				return err
			}
		}
	} else {
		hs.hello = hello
	}

	if _, err := hs.c.writeHandshakeRecord(hs.hello, hs.transcript); err != nil {
		return err
	}

	// serverHelloMsg is not included in the transcript
	msg, err := c.readHandshake(nil)
	if err != nil {
		return err
	}

	serverHello, ok := msg.(*serverHelloMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(serverHello, msg)
	}
	hs.serverHello = serverHello

	if err := hs.checkServerHelloOrHRR(); err != nil {
		return err
	}

	c.didHRR = true
	return nil
}

func (hs *clientHandshakeStateTLS13) processServerHello() error {
	c := hs.c

	if bytes.Equal(hs.serverHello.random, helloRetryRequestRandom) {
		c.sendAlert(alertUnexpectedMessage)
		return errors.New("tls: server sent two HelloRetryRequest messages")
	}

	if len(hs.serverHello.cookie) != 0 {
		c.sendAlert(alertUnsupportedExtension)
		return errors.New("tls: server sent a cookie in a normal ServerHello")
	}

	if hs.serverHello.selectedGroup != 0 {
		c.sendAlert(alertDecodeError)
		return errors.New("tls: malformed key_share extension")
	}

	if hs.serverHello.serverShare.group == 0 {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server did not send a key share")
	}
	if !slices.ContainsFunc(hs.hello.keyShares, func(ks keyShare) bool {
		return ks.group == hs.serverHello.serverShare.group
	}) {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server selected unsupported group")
	}

	if !hs.serverHello.selectedIdentityPresent {
		return nil
	}

	if int(hs.serverHello.selectedIdentity) >= len(hs.hello.pskIdentities) {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server selected an invalid PSK")
	}

	if len(hs.hello.pskIdentities) != 1 || hs.session == nil {
		return c.sendAlert(alertInternalError)
	}
	pskSuite := cipherSuiteTLS13ByID(hs.session.cipherSuite)
	if pskSuite == nil {
		return c.sendAlert(alertInternalError)
	}
	if pskSuite.hash != hs.suite.hash {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server selected an invalid PSK and cipher suite pair")
	}

	hs.usingPSK = true
	c.didResume = true
	c.peerCertificates = hs.session.peerCertificates
	c.activeCertHandles = hs.session.activeCertHandles
	c.verifiedChains = hs.session.verifiedChains
	c.ocspResponse = hs.session.ocspResponse
	c.scts = hs.session.scts
	return nil
}

// [uTLS] SECTION BEGIN
func getSharedKey(peerData []byte, key *ecdh.PrivateKey) ([]byte, error) {
	peerKey, err := key.Curve().NewPublicKey(peerData)
	if err != nil {
		return nil, errors.New("tls: invalid server key share")
	}
	sharedKey, err := key.ECDH(peerKey)
	if err != nil {
		return nil, errors.New("tls: invalid server key share")
	}

	return sharedKey, nil
}

// [uTLS] SECTION END

func (hs *clientHandshakeStateTLS13) establishHandshakeKeys() error {
	c := hs.c

	ecdhePeerData := hs.serverHello.serverShare.data
	if hs.serverHello.serverShare.group == X25519MLKEM768 {
		if len(ecdhePeerData) != mlkem.CiphertextSize768+x25519PublicKeySize {
			c.sendAlert(alertIllegalParameter)
			return errors.New("tls: invalid server X25519MLKEM768 key share")
		}
		ecdhePeerData = hs.serverHello.serverShare.data[mlkem.CiphertextSize768:]
	}
	// [uTLS] SECTION BEGIN
	if hs.serverHello.serverShare.group == X25519Kyber768Draft00 {
		if len(ecdhePeerData) != x25519PublicKeySize+mlkem.CiphertextSize768 {
			c.sendAlert(alertIllegalParameter)
			return errors.New("tls: invalid server X25519Kyber768Draft00 key share")
		}
		ecdhePeerData = hs.serverHello.serverShare.data[:x25519PublicKeySize]
	}
	sharedKey, err := getSharedKey(ecdhePeerData, hs.keyShareKeys.ecdhe)
	// [uTLS] SECTION END
	if err != nil {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: invalid server key share")
	}
	if hs.serverHello.serverShare.group == X25519MLKEM768 {
		if hs.keyShareKeys.mlkem == nil {
			return c.sendAlert(alertInternalError)
		}
		// [uTLS] SECTION BEGIN
		if hs.uconn != nil && hs.uconn.clientHelloBuildStatus == BuildByUtls {
			if sharedKey, err = getSharedKey(ecdhePeerData, hs.keyShareKeys.mlkemEcdhe); err != nil {
				c.sendAlert(alertIllegalParameter)
				return errors.New("tls: invalid server key share")
			}
		}
		// [uTLS] SECTION END
		ciphertext := hs.serverHello.serverShare.data[:mlkem.CiphertextSize768]
		mlkemShared, err := hs.keyShareKeys.mlkem.Decapsulate(ciphertext)
		if err != nil {
			c.sendAlert(alertIllegalParameter)
			return errors.New("tls: invalid X25519MLKEM768 server key share")
		}
		sharedKey = append(mlkemShared, sharedKey...)
	}
	// [uTLS] SECTION BEGIN
	if hs.serverHello.serverShare.group == X25519Kyber768Draft00 {
		if hs.keyShareKeys.mlkem == nil {
			return c.sendAlert(alertInternalError)
		}
		if hs.uconn != nil && hs.uconn.clientHelloBuildStatus == BuildByUtls {
			if sharedKey, err = getSharedKey(ecdhePeerData, hs.keyShareKeys.mlkemEcdhe); err != nil {
				c.sendAlert(alertIllegalParameter)
				return errors.New("tls: invalid server key share")
			}
		}
		ciphertext := hs.serverHello.serverShare.data[x25519PublicKeySize:]
		kyberShared, err := kyberDecapsulate(hs.keyShareKeys.mlkem, ciphertext)
		if err != nil {
			c.sendAlert(alertIllegalParameter)
			return errors.New("tls: invalid X25519Kyber768Draft00 server key share")
		}
		sharedKey = append(sharedKey, kyberShared...)
	}
	// [uTLS] SECTION END
	c.curveID = hs.serverHello.serverShare.group

	earlySecret := hs.earlySecret
	if !hs.usingPSK {
		earlySecret = tls13.NewEarlySecret(hs.suite.hash.New, nil)
	}

	handshakeSecret := earlySecret.HandshakeSecret(sharedKey)

	clientSecret := handshakeSecret.ClientHandshakeTrafficSecret(hs.transcript)
	c.out.setTrafficSecret(hs.suite, QUICEncryptionLevelHandshake, clientSecret)
	serverSecret := handshakeSecret.ServerHandshakeTrafficSecret(hs.transcript)
	c.in.setTrafficSecret(hs.suite, QUICEncryptionLevelHandshake, serverSecret)

	if c.quic != nil {
		if c.hand.Len() != 0 {
			c.sendAlert(alertUnexpectedMessage)
		}
		c.quicSetWriteSecret(QUICEncryptionLevelHandshake, hs.suite.id, clientSecret)
		c.quicSetReadSecret(QUICEncryptionLevelHandshake, hs.suite.id, serverSecret)
	}

	err = c.config.writeKeyLog(keyLogLabelClientHandshake, hs.hello.random, clientSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}
	err = c.config.writeKeyLog(keyLogLabelServerHandshake, hs.hello.random, serverSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}

	hs.masterSecret = handshakeSecret.MasterSecret()

	return nil
}

func (hs *clientHandshakeStateTLS13) readServerParameters() error {
	c := hs.c

	msg, err := c.readHandshake(hs.transcript)
	if err != nil {
		return err
	}

	encryptedExtensions, ok := msg.(*encryptedExtensionsMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(encryptedExtensions, msg)
	}

	if err := checkALPN(hs.hello.alpnProtocols, encryptedExtensions.alpnProtocol, c.quic != nil); err != nil {
		// RFC 8446 specifies that no_application_protocol is sent by servers, but
		// does not specify how clients handle the selection of an incompatible protocol.
		// RFC 9001 Section 8.1 specifies that QUIC clients send no_application_protocol
		// in this case. Always sending no_application_protocol seems reasonable.
		c.sendAlert(alertNoApplicationProtocol)
		return err
	}
	c.clientProtocol = encryptedExtensions.alpnProtocol

	// [UTLS SECTION STARTS]
	if hs.uconn != nil {
		err = hs.utlsReadServerParameters(encryptedExtensions)
		if err != nil {
			c.sendAlert(alertUnsupportedExtension)
			return err
		}
	}
	// [UTLS SECTION ENDS]

	if c.quic != nil {
		if encryptedExtensions.quicTransportParameters == nil {
			// RFC 9001 Section 8.2.
			c.sendAlert(alertMissingExtension)
			return errors.New("tls: server did not send a quic_transport_parameters extension")
		}
		c.quicSetTransportParameters(encryptedExtensions.quicTransportParameters)
	} else {
		if encryptedExtensions.quicTransportParameters != nil {
			c.sendAlert(alertUnsupportedExtension)
			return errors.New("tls: server sent an unexpected quic_transport_parameters extension")
		}
	}

	if !hs.hello.earlyData && encryptedExtensions.earlyData {
		c.sendAlert(alertUnsupportedExtension)
		return errors.New("tls: server sent an unexpected early_data extension")
	}
	if hs.hello.earlyData && !encryptedExtensions.earlyData {
		c.quicRejectedEarlyData()
	}
	if encryptedExtensions.earlyData {
		if hs.session.cipherSuite != c.cipherSuite {
			c.sendAlert(alertHandshakeFailure)
			return errors.New("tls: server accepted 0-RTT with the wrong cipher suite")
		}
		if hs.session.alpnProtocol != c.clientProtocol {
			c.sendAlert(alertHandshakeFailure)
			return errors.New("tls: server accepted 0-RTT with the wrong ALPN")
		}
	}
	if hs.echContext != nil {
		if hs.echContext.echRejected {
			hs.echContext.retryConfigs = encryptedExtensions.echRetryConfigs
		} else if encryptedExtensions.echRetryConfigs != nil {
			c.sendAlert(alertUnsupportedExtension)
			return errors.New("tls: server sent encrypted client hello retry configs after accepting encrypted client hello")
		}
	}

	return nil
}

func (hs *clientHandshakeStateTLS13) readServerCertificate() error {
	c := hs.c

	// Either a PSK or a certificate is always used, but not both.
	// See RFC 8446, Section 4.1.1.
	if hs.usingPSK {
		// Make sure the connection is still being verified whether or not this
		// is a resumption. Resumptions currently don't reverify certificates so
		// they don't call verifyServerCertificate. See Issue 31641.
		if c.config.VerifyConnection != nil {
			if err := c.config.VerifyConnection(c.connectionStateLocked()); err != nil {
				c.sendAlert(alertBadCertificate)
				return err
			}
		}
		return nil
	}

	// [UTLS SECTION BEGINS]
	// msg, err := c.readHandshake(hs.transcript)
	msg, err := c.readHandshake(nil) // hold writing to transcript until we know it is not compressed cert
	// [UTLS SECTION ENDS]
	if err != nil {
		return err
	}

	certReq, ok := msg.(*certificateRequestMsgTLS13)
	if ok {
		hs.certReq = certReq
		transcriptMsg(certReq, hs.transcript) // [UTLS] if it is certReq (not compressedCert), write to transcript

		// msg, err = c.readHandshake(hs.transcript)
		msg, err = c.readHandshake(nil) // [UTLS] we don't write to transcript until make sure it is not compressed cert
		if err != nil {
			return err
		}
	}

	// [UTLS SECTION BEGINS]
	var skipWritingCertToTranscript bool = false
	if hs.uconn != nil {
		processedMsg, err := hs.utlsReadServerCertificate(msg)
		if err != nil {
			return err
		}
		if processedMsg != nil {
			skipWritingCertToTranscript = true
			msg = processedMsg // msg is now a processed-by-extension certificateMsg
		}
	}
	// [UTLS SECTION ENDS]

	certMsg, ok := msg.(*certificateMsgTLS13)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(certMsg, msg)
	}
	if len(certMsg.certificate.Certificate) == 0 {
		c.sendAlert(alertDecodeError)
		return errors.New("tls: received empty certificates message")
	}
	// [UTLS SECTION BEGINS]
	if !skipWritingCertToTranscript { // write to transcript only if it is not compressedCert (i.e. if not processed by extension)
		if err = transcriptMsg(certMsg, hs.transcript); err != nil {
			return err
		}
	}
	// [UTLS SECTION ENDS]

	c.scts = certMsg.certificate.SignedCertificateTimestamps
	c.ocspResponse = certMsg.certificate.OCSPStaple

	if err := c.verifyServerCertificate(certMsg.certificate.Certificate); err != nil {
		return err
	}

	// certificateVerifyMsg is included in the transcript, but not until
	// after we verify the handshake signature, since the state before
	// this message was sent is used.
	msg, err = c.readHandshake(nil)
	if err != nil {
		return err
	}

	certVerify, ok := msg.(*certificateVerifyMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(certVerify, msg)
	}

	// See RFC 8446, Section 4.4.3.
	if !isSupportedSignatureAlgorithm(certVerify.signatureAlgorithm, supportedSignatureAlgorithms()) {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: certificate used with invalid signature algorithm")
	}
	sigType, sigHash, err := typeAndHashFromSignatureScheme(certVerify.signatureAlgorithm)
	if err != nil {
		return c.sendAlert(alertInternalError)
	}
	if sigType == signaturePKCS1v15 || sigHash == crypto.SHA1 {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: certificate used with invalid signature algorithm")
	}
	signed := signedMessage(sigHash, serverSignatureContext, hs.transcript)
	if err := verifyHandshakeSignature(sigType, c.peerCertificates[0].PublicKey,
		sigHash, signed, certVerify.signature); err != nil {
		c.sendAlert(alertDecryptError)
		return errors.New("tls: invalid signature by the server certificate: " + err.Error())
	}

	if err := transcriptMsg(certVerify, hs.transcript); err != nil {
		return err
	}

	return nil
}

func (hs *clientHandshakeStateTLS13) readServerFinished() error {
	c := hs.c

	// finishedMsg is included in the transcript, but not until after we
	// check the client version, since the state before this message was
	// sent is used during verification.
	msg, err := c.readHandshake(nil)
	if err != nil {
		return err
	}

	finished, ok := msg.(*finishedMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(finished, msg)
	}

	expectedMAC := hs.suite.finishedHash(c.in.trafficSecret, hs.transcript)
	if !hmac.Equal(expectedMAC, finished.verifyData) {
		c.sendAlert(alertDecryptError)
		return errors.New("tls: invalid server finished hash")
	}

	if err := transcriptMsg(finished, hs.transcript); err != nil {
		return err
	}

	// Derive secrets that take context through the server Finished.

	hs.trafficSecret = hs.masterSecret.ClientApplicationTrafficSecret(hs.transcript)
	serverSecret := hs.masterSecret.ServerApplicationTrafficSecret(hs.transcript)
	c.in.setTrafficSecret(hs.suite, QUICEncryptionLevelApplication, serverSecret)

	err = c.config.writeKeyLog(keyLogLabelClientTraffic, hs.hello.random, hs.trafficSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}
	err = c.config.writeKeyLog(keyLogLabelServerTraffic, hs.hello.random, serverSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}

	c.ekm = hs.suite.exportKeyingMaterial(hs.masterSecret, hs.transcript)

	return nil
}

func (hs *clientHandshakeStateTLS13) sendClientCertificate() error {
	c := hs.c

	if hs.certReq == nil {
		return nil
	}

	if hs.echContext != nil && hs.echContext.echRejected {
		if _, err := hs.c.writeHandshakeRecord(&certificateMsgTLS13{}, hs.transcript); err != nil {
			return err
		}
		return nil
	}

	cert, err := c.getClientCertificate(&CertificateRequestInfo{
		AcceptableCAs:    hs.certReq.certificateAuthorities,
		SignatureSchemes: hs.certReq.supportedSignatureAlgorithms,
		Version:          c.vers,
		ctx:              hs.ctx,
	})
	if err != nil {
		return err
	}

	certMsg := new(certificateMsgTLS13)

	certMsg.certificate = *cert
	certMsg.scts = hs.certReq.scts && len(cert.SignedCertificateTimestamps) > 0
	certMsg.ocspStapling = hs.certReq.ocspStapling && len(cert.OCSPStaple) > 0

	if _, err := hs.c.writeHandshakeRecord(certMsg, hs.transcript); err != nil {
		return err
	}

	// If we sent an empty certificate message, skip the CertificateVerify.
	if len(cert.Certificate) == 0 {
		return nil
	}

	certVerifyMsg := new(certificateVerifyMsg)
	certVerifyMsg.hasSignatureAlgorithm = true

	certVerifyMsg.signatureAlgorithm, err = selectSignatureScheme(c.vers, cert, hs.certReq.supportedSignatureAlgorithms)
	if err != nil {
		// getClientCertificate returned a certificate incompatible with the
		// CertificateRequestInfo supported signature algorithms.
		c.sendAlert(alertHandshakeFailure)
		return err
	}

	sigType, sigHash, err := typeAndHashFromSignatureScheme(certVerifyMsg.signatureAlgorithm)
	if err != nil {
		return c.sendAlert(alertInternalError)
	}

	signed := signedMessage(sigHash, clientSignatureContext, hs.transcript)
	signOpts := crypto.SignerOpts(sigHash)
	if sigType == signatureRSAPSS {
		signOpts = &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: sigHash}
	}
	sig, err := cert.PrivateKey.(crypto.Signer).Sign(c.config.rand(), signed, signOpts)
	if err != nil {
		c.sendAlert(alertInternalError)
		return errors.New("tls: failed to sign handshake: " + err.Error())
	}
	certVerifyMsg.signature = sig

	if _, err := hs.c.writeHandshakeRecord(certVerifyMsg, hs.transcript); err != nil {
		return err
	}

	return nil
}

func (hs *clientHandshakeStateTLS13) sendClientFinished() error {
	c := hs.c

	finished := &finishedMsg{
		verifyData: hs.suite.finishedHash(c.out.trafficSecret, hs.transcript),
	}

	if _, err := hs.c.writeHandshakeRecord(finished, hs.transcript); err != nil {
		return err
	}

	c.out.setTrafficSecret(hs.suite, QUICEncryptionLevelApplication, hs.trafficSecret)

	if !c.config.SessionTicketsDisabled && c.config.ClientSessionCache != nil {
		c.resumptionSecret = hs.masterSecret.ResumptionMasterSecret(hs.transcript)
	}

	if c.quic != nil {
		if c.hand.Len() != 0 {
			c.sendAlert(alertUnexpectedMessage)
		}
		c.quicSetWriteSecret(QUICEncryptionLevelApplication, hs.suite.id, hs.trafficSecret)
	}

	return nil
}

func (c *Conn) handleNewSessionTicket(msg *newSessionTicketMsgTLS13) error {
	if !c.isClient {
		c.sendAlert(alertUnexpectedMessage)
		return errors.New("tls: received new session ticket from a client")
	}

	if c.config.SessionTicketsDisabled || c.config.ClientSessionCache == nil {
		return nil
	}

	// See RFC 8446, Section 4.6.1.
	if msg.lifetime == 0 {
		return nil
	}
	lifetime := time.Duration(msg.lifetime) * time.Second
	if lifetime > maxSessionTicketLifetime {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: received a session ticket with invalid lifetime")
	}

	// RFC 9001, Section 4.6.1
	if c.quic != nil && msg.maxEarlyData != 0 && msg.maxEarlyData != 0xffffffff {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: invalid early data for QUIC connection")
	}

	cipherSuite := cipherSuiteTLS13ByID(c.cipherSuite)
	if cipherSuite == nil || c.resumptionSecret == nil {
		return c.sendAlert(alertInternalError)
	}

	psk := tls13.ExpandLabel(cipherSuite.hash.New, c.resumptionSecret, "resumption",
		msg.nonce, cipherSuite.hash.Size())

	session := c.sessionState()
	session.secret = psk
	session.useBy = uint64(c.config.time().Add(lifetime).Unix())
	session.ageAdd = msg.ageAdd
	session.EarlyData = c.quic != nil && msg.maxEarlyData == 0xffffffff // RFC 9001, Section 4.6.1
	session.ticket = msg.label
	if c.quic != nil && c.quic.enableSessionEvents {
		c.quicStoreSession(session)
		return nil
	}
	cs := &ClientSessionState{session: session}
	if cacheKey := c.clientSessionCacheKey(); cacheKey != "" {
		c.config.ClientSessionCache.Put(cacheKey, cs)
	}

	return nil
}

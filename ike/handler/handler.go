// SPDX-FileCopyrightText: 2024 Intel Corporation
// Copyright 2019 free5GC.org
//
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	mathRand "math/rand"
	"net"
	"sync/atomic"

	"github.com/omec-project/n3iwf/context"
	ike_message "github.com/omec-project/n3iwf/ike/message"
	"github.com/omec-project/n3iwf/ike/xfrm"
	"github.com/omec-project/n3iwf/logger"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func HandleIKESAINIT(udpConn *net.UDPConn, n3iwfAddr, ueAddr *net.UDPAddr, message *ike_message.IKEMessage, realMessage1 []byte) {
	logger.IKELog.Infoln("handle IKE_SA_INIT")

	// Used to receive value from peer
	var securityAssociation *ike_message.SecurityAssociation
	var keyExcahge *ike_message.KeyExchange
	var nonce *ike_message.Nonce
	var notifications []*ike_message.Notification

	n3iwfSelf := context.N3IWFSelf()

	// For response or needed data
	responseIKEMessage := new(ike_message.IKEMessage)
	var sharedKeyData, localNonce, concatenatedNonce []byte
	// Chosen transform from peer's proposal
	var encryptionAlgorithmTransform, pseudorandomFunctionTransform *ike_message.Transform
	var integrityAlgorithmTransform, diffieHellmanGroupTransform *ike_message.Transform
	// For NAT-T
	var ueIsBehindNAT, n3iwfIsBehindNAT bool

	if message == nil {
		logger.IKELog.Errorln("IKE Message is nil")
		return
	}

	// parse IKE header and setup IKE context
	// check major version
	majorVersion := ((message.Version & 0xf0) >> 4)
	if majorVersion > 2 {
		logger.IKELog.Warnln("received an IKE message with higher major version")
		// send INFORMATIONAL type message with INVALID_MAJOR_VERSION Notify payload
		responseIKEMessage.BuildIKEHeader(message.InitiatorSPI, message.ResponderSPI,
			ike_message.INFORMATIONAL, ike_message.ResponseBitCheck, message.MessageID)
		responseIKEMessage.Payloads.Reset()
		responseIKEMessage.Payloads.BuildNotification(ike_message.TypeNone,
			ike_message.INVALID_MAJOR_VERSION, nil, nil)

		SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)

		return
	}

	for _, ikePayload := range message.Payloads {
		switch ikePayload.Type() {
		case ike_message.TypeSA:
			securityAssociation = ikePayload.(*ike_message.SecurityAssociation)
		case ike_message.TypeKE:
			keyExcahge = ikePayload.(*ike_message.KeyExchange)
		case ike_message.TypeNiNr:
			nonce = ikePayload.(*ike_message.Nonce)
		case ike_message.TypeN:
			notifications = append(notifications, ikePayload.(*ike_message.Notification))
		default:
			logger.IKELog.Warnf(
				"get IKE payload (type %d) in IKE_SA_INIT message, this payload will not be handled by IKE handler",
				ikePayload.Type())
		}
	}

	if securityAssociation == nil {
		logger.IKELog.Errorln("the security association field is nil")
		// TODO: send error message to UE
		return
	}
	responseSecurityAssociation := responseIKEMessage.Payloads.BuildSecurityAssociation()

	for _, proposal := range securityAssociation.Proposals {
		// We need ENCR, PRF, INTEG, DH, but not ESN
		encryptionAlgorithmTransform = nil
		pseudorandomFunctionTransform = nil
		integrityAlgorithmTransform = nil
		diffieHellmanGroupTransform = nil

		if len(proposal.EncryptionAlgorithm) > 0 {
			for _, transform := range proposal.EncryptionAlgorithm {
				if is_supported(ike_message.TypeEncryptionAlgorithm, transform.TransformID,
					transform.AttributePresent, transform.AttributeValue) {
					encryptionAlgorithmTransform = transform
					break
				}
			}
			if encryptionAlgorithmTransform == nil {
				continue
			}
		} else {
			continue // mandatory
		}
		if len(proposal.PseudorandomFunction) > 0 {
			for _, transform := range proposal.PseudorandomFunction {
				if is_supported(ike_message.TypePseudorandomFunction, transform.TransformID,
					transform.AttributePresent, transform.AttributeValue) {
					pseudorandomFunctionTransform = transform
					break
				}
			}
			if pseudorandomFunctionTransform == nil {
				continue
			}
		} else {
			continue // mandatory
		}
		if len(proposal.IntegrityAlgorithm) > 0 {
			for _, transform := range proposal.IntegrityAlgorithm {
				if is_supported(ike_message.TypeIntegrityAlgorithm, transform.TransformID,
					transform.AttributePresent, transform.AttributeValue) {
					integrityAlgorithmTransform = transform
					break
				}
			}
			if integrityAlgorithmTransform == nil {
				continue
			}
		} else {
			continue // mandatory
		}
		if len(proposal.DiffieHellmanGroup) > 0 {
			for _, transform := range proposal.DiffieHellmanGroup {
				if is_supported(ike_message.TypeDiffieHellmanGroup, transform.TransformID,
					transform.AttributePresent, transform.AttributeValue) {
					diffieHellmanGroupTransform = transform
					break
				}
			}
			if diffieHellmanGroupTransform == nil {
				continue
			}
		} else {
			continue // mandatory
		}
		if len(proposal.ExtendedSequenceNumbers) > 0 {
			continue // No ESN
		}

		// Construct chosen proposal, with ENCR, PRF, INTEG, DH, and each
		// contains one transform expectively
		chosenProposal := responseSecurityAssociation.Proposals.BuildProposal(
			proposal.ProposalNumber, proposal.ProtocolID, nil)
		chosenProposal.EncryptionAlgorithm = append(chosenProposal.EncryptionAlgorithm, encryptionAlgorithmTransform)
		chosenProposal.PseudorandomFunction = append(chosenProposal.PseudorandomFunction, pseudorandomFunctionTransform)
		chosenProposal.IntegrityAlgorithm = append(chosenProposal.IntegrityAlgorithm, integrityAlgorithmTransform)
		chosenProposal.DiffieHellmanGroup = append(chosenProposal.DiffieHellmanGroup, diffieHellmanGroupTransform)

		break
	}

	if len(responseSecurityAssociation.Proposals) == 0 {
		logger.IKELog.Warnln("no proposal chosen")
		// Respond NO_PROPOSAL_CHOSEN to UE
		responseIKEMessage.BuildIKEHeader(message.InitiatorSPI, message.ResponderSPI,
			ike_message.IKE_SA_INIT, ike_message.ResponseBitCheck, message.MessageID)
		responseIKEMessage.Payloads.Reset()
		responseIKEMessage.Payloads.BuildNotification(ike_message.TypeNone, ike_message.NO_PROPOSAL_CHOSEN, nil, nil)

		SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)

		return
	}

	if keyExcahge == nil {
		logger.IKELog.Errorln("the key exchange field is nil")
		// TODO: send error message to UE
		return
	}
	chosenDiffieHellmanGroup := diffieHellmanGroupTransform.TransformID
	if chosenDiffieHellmanGroup != keyExcahge.DiffieHellmanGroup {
		logger.IKELog.Warnln("the Diffie-Hellman group defined in key exchange payload not matches the one in chosen proposal")
		// send INVALID_KE_PAYLOAD to UE
		responseIKEMessage.BuildIKEHeader(message.InitiatorSPI, message.ResponderSPI,
			ike_message.IKE_SA_INIT, ike_message.ResponseBitCheck, message.MessageID)
		responseIKEMessage.Payloads.Reset()

		notificationData := make([]byte, 2)
		binary.BigEndian.PutUint16(notificationData, chosenDiffieHellmanGroup)
		responseIKEMessage.Payloads.BuildNotification(
			ike_message.TypeNone, ike_message.INVALID_KE_PAYLOAD, nil, notificationData)

		SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)

		return
	}

	var localPublicValue []byte

	localPublicValue, sharedKeyData = CalculateDiffieHellmanMaterials(GenerateRandomNumber(),
		keyExcahge.KeyExchangeData, chosenDiffieHellmanGroup)
	responseIKEMessage.Payloads.BuildKeyExchange(chosenDiffieHellmanGroup, localPublicValue)

	if nonce == nil {
		logger.IKELog.Errorln("the nonce field is nil")
		// TODO: send error message to UE
		return
	}
	localNonce = GenerateRandomNumber().Bytes()
	concatenatedNonce = append(nonce.NonceData, localNonce...)

	responseIKEMessage.Payloads.BuildNonce(localNonce)

	if len(notifications) != 0 {
		for _, notification := range notifications {
			switch notification.NotifyMessageType {
			case ike_message.NAT_DETECTION_SOURCE_IP:
				logger.IKELog.Debugln("received IKE Notify: NAT_DETECTION_SOURCE_IP")
				// Calculate local NAT_DETECTION_SOURCE_IP hash
				// : sha1(ispi | rspi | ueip | ueport)
				localDetectionData := make([]byte, 22)
				binary.BigEndian.PutUint64(localDetectionData[0:8], message.InitiatorSPI)
				binary.BigEndian.PutUint64(localDetectionData[8:16], message.ResponderSPI)
				copy(localDetectionData[16:20], ueAddr.IP.To4())
				binary.BigEndian.PutUint16(localDetectionData[20:22], uint16(ueAddr.Port))

				sha1HashFunction := sha1.New()
				if _, err := sha1HashFunction.Write(localDetectionData); err != nil {
					logger.IKELog.Errorf("hash function write error: %+v", err)
					return
				}

				if !bytes.Equal(notification.NotificationData, sha1HashFunction.Sum(nil)) {
					ueIsBehindNAT = true
				}
			case ike_message.NAT_DETECTION_DESTINATION_IP:
				logger.IKELog.Debugln("received IKE Notify: NAT_DETECTION_DESTINATION_IP")
				// Calculate local NAT_DETECTION_SOURCE_IP hash
				// : sha1(ispi | rspi | n3iwfip | n3iwfport)
				localDetectionData := make([]byte, 22)
				binary.BigEndian.PutUint64(localDetectionData[0:8], message.InitiatorSPI)
				binary.BigEndian.PutUint64(localDetectionData[8:16], message.ResponderSPI)
				copy(localDetectionData[16:20], n3iwfAddr.IP.To4())
				binary.BigEndian.PutUint16(localDetectionData[20:22], uint16(n3iwfAddr.Port))

				sha1HashFunction := sha1.New()
				if _, err := sha1HashFunction.Write(localDetectionData); err != nil {
					logger.IKELog.Errorf("hash function write error: %+v", err)
					return
				}

				if !bytes.Equal(notification.NotificationData, sha1HashFunction.Sum(nil)) {
					n3iwfIsBehindNAT = true
				}
			default:
			}
		}
	}

	// Create new IKE security association
	ikeSecurityAssociation := n3iwfSelf.NewIKESecurityAssociation()
	ikeSecurityAssociation.RemoteSPI = message.InitiatorSPI
	ikeSecurityAssociation.InitiatorMessageID = message.MessageID
	ikeSecurityAssociation.UEIsBehindNAT = ueIsBehindNAT
	ikeSecurityAssociation.N3IWFIsBehindNAT = n3iwfIsBehindNAT

	// Record algorithm in context
	ikeSecurityAssociation.EncryptionAlgorithm = encryptionAlgorithmTransform
	ikeSecurityAssociation.IntegrityAlgorithm = integrityAlgorithmTransform
	ikeSecurityAssociation.PseudorandomFunction = pseudorandomFunctionTransform
	ikeSecurityAssociation.DiffieHellmanGroup = diffieHellmanGroupTransform

	// Record concatenated nonce
	ikeSecurityAssociation.ConcatenatedNonce = append(ikeSecurityAssociation.ConcatenatedNonce, concatenatedNonce...)
	// Record Diffie-Hellman shared key
	ikeSecurityAssociation.DiffieHellmanSharedKey = append(ikeSecurityAssociation.DiffieHellmanSharedKey, sharedKeyData...)

	if err := GenerateKeyForIKESA(ikeSecurityAssociation); err != nil {
		logger.IKELog.Errorf("generate key for IKE SA failed: %+v", err)
		return
	}

	// IKE response to UE
	responseIKEMessage.BuildIKEHeader(ikeSecurityAssociation.RemoteSPI, ikeSecurityAssociation.LocalSPI,
		ike_message.IKE_SA_INIT, ike_message.ResponseBitCheck, message.MessageID)

	// Calculate NAT_DETECTION_SOURCE_IP for NAT-T
	natDetectionSourceIP := make([]byte, 22)
	binary.BigEndian.PutUint64(natDetectionSourceIP[0:8], ikeSecurityAssociation.RemoteSPI)
	binary.BigEndian.PutUint64(natDetectionSourceIP[8:16], ikeSecurityAssociation.LocalSPI)
	copy(natDetectionSourceIP[16:20], n3iwfAddr.IP.To4())
	binary.BigEndian.PutUint16(natDetectionSourceIP[20:22], uint16(n3iwfAddr.Port))

	// Build and append notify payload for NAT_DETECTION_SOURCE_IP
	responseIKEMessage.Payloads.BuildNotification(
		ike_message.TypeNone, ike_message.NAT_DETECTION_SOURCE_IP, nil, natDetectionSourceIP)

	// Calculate NAT_DETECTION_DESTINATION_IP for NAT-T
	natDetectionDestinationIP := make([]byte, 22)
	binary.BigEndian.PutUint64(natDetectionDestinationIP[0:8], ikeSecurityAssociation.RemoteSPI)
	binary.BigEndian.PutUint64(natDetectionDestinationIP[8:16], ikeSecurityAssociation.LocalSPI)
	copy(natDetectionDestinationIP[16:20], ueAddr.IP.To4())
	binary.BigEndian.PutUint16(natDetectionDestinationIP[20:22], uint16(ueAddr.Port))

	// Build and append notify payload for NAT_DETECTION_DESTINATION_IP
	responseIKEMessage.Payloads.BuildNotification(
		ike_message.TypeNone, ike_message.NAT_DETECTION_DESTINATION_IP, nil, natDetectionDestinationIP)

	// Prepare authentication data - InitatorSignedOctet
	// InitatorSignedOctet = RealMessage1 | NonceRData | MACedIDForI
	// MACedIDForI is acquired in IKE_AUTH exchange
	ikeSecurityAssociation.InitiatorSignedOctets = append(realMessage1, localNonce...)

	// Prepare authentication data - ResponderSignedOctet
	// ResponderSignedOctet = RealMessage2 | NonceIData | MACedIDForR
	responseIKEMessageData, err := responseIKEMessage.Encode()
	if err != nil {
		logger.IKELog.Errorln("encoding IKE message failed: %+v", err)
		return
	}
	ikeSecurityAssociation.ResponderSignedOctets = append(responseIKEMessageData, nonce.NonceData...)
	// MACedIDForR
	var idPayload ike_message.IKEPayloadContainer
	idPayload.BuildIdentificationResponder(ike_message.ID_FQDN, []byte(n3iwfSelf.Fqdn))
	idPayloadData, err := idPayload.Encode()
	if err != nil {
		logger.IKELog.Errorln("encode IKE payload failed: %+v", err)
		return
	}
	pseudorandomFunction, ok := NewPseudorandomFunction(ikeSecurityAssociation.SK_pr,
		ikeSecurityAssociation.PseudorandomFunction.TransformID)
	if !ok {
		logger.IKELog.Errorln("get an unsupported pseudorandom funcion. This may imply an unsupported transform is chosen")
		return
	}
	if _, err := pseudorandomFunction.Write(idPayloadData[4:]); err != nil {
		logger.IKELog.Errorf("pseudorandom function write error: %+v", err)
		return
	}
	ikeSecurityAssociation.ResponderSignedOctets = append(ikeSecurityAssociation.ResponderSignedOctets,
		pseudorandomFunction.Sum(nil)...)

	logger.IKELog.Debugf("local unsigned authentication data:\n%s", hex.Dump(ikeSecurityAssociation.ResponderSignedOctets))

	// Send response to UE
	SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)
}

// IKE_AUTH state
const (
	PreSignalling = iota
	EAPSignalling
	PostSignalling
	EndSignalling
	HandleCreateChildSA
)

func HandleIKEAUTH(udpConn *net.UDPConn, n3iwfAddr, ueAddr *net.UDPAddr, message *ike_message.IKEMessage) {
	logger.IKELog.Debugln("handle IKE_AUTH")

	var encryptedPayload *ike_message.Encrypted

	n3iwfSelf := context.N3IWFSelf()

	// Used for response
	responseIKEMessage := new(ike_message.IKEMessage)
	var responseIKEPayload ike_message.IKEPayloadContainer

	if message == nil {
		logger.IKELog.Error("IKE Message is nil")
		return
	}

	// parse IKE header and setup IKE context
	// check major version
	majorVersion := ((message.Version & 0xf0) >> 4)
	if majorVersion > 2 {
		logger.IKELog.Warnln("received an IKE message with higher major version")
		// send INFORMATIONAL type message with INVALID_MAJOR_VERSION Notify payload ( OUTSIDE IKE SA )
		responseIKEMessage.BuildIKEHeader(message.InitiatorSPI, message.ResponderSPI,
			ike_message.INFORMATIONAL, ike_message.ResponseBitCheck, message.MessageID)
		responseIKEMessage.Payloads.Reset()
		responseIKEMessage.Payloads.BuildNotification(ike_message.TypeNone, ike_message.INVALID_MAJOR_VERSION, nil, nil)

		SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)

		return
	}

	// Find corresponding IKE security association
	localSPI := message.ResponderSPI
	ikeSecurityAssociation, ok := n3iwfSelf.IKESALoad(localSPI)
	if !ok {
		logger.IKELog.Warnln("unrecognized SPI")
		// send INFORMATIONAL type message with INVALID_IKE_SPI Notify payload ( OUTSIDE IKE SA )
		responseIKEMessage.BuildIKEHeader(message.InitiatorSPI, 0, ike_message.INFORMATIONAL,
			ike_message.ResponseBitCheck, message.MessageID)
		responseIKEMessage.Payloads.Reset()
		responseIKEMessage.Payloads.BuildNotification(ike_message.TypeNone, ike_message.INVALID_IKE_SPI, nil, nil)

		SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)

		return
	}

	for _, ikePayload := range message.Payloads {
		switch ikePayload.Type() {
		case ike_message.TypeSK:
			encryptedPayload = ikePayload.(*ike_message.Encrypted)
		default:
			logger.IKELog.Warnf(
				"get IKE payload (type %d) in IKE_AUTH message, this payload will not be handled by IKE handler",
				ikePayload.Type())
		}
	}

	decryptedIKEPayload, err := DecryptProcedure(ikeSecurityAssociation, message, encryptedPayload)
	if err != nil {
		logger.IKELog.Errorf("decrypt IKE message failed: %+v", err)
		return
	}

	// Parse payloads
	var initiatorID *ike_message.IdentificationInitiator
	var certificateRequest *ike_message.CertificateRequest
	var certificate *ike_message.Certificate
	var securityAssociation *ike_message.SecurityAssociation
	var trafficSelectorInitiator *ike_message.TrafficSelectorInitiator
	var trafficSelectorResponder *ike_message.TrafficSelectorResponder
	var eap *ike_message.EAP
	var authentication *ike_message.Authentication
	var configuration *ike_message.Configuration

	for _, ikePayload := range decryptedIKEPayload {
		switch ikePayload.Type() {
		case ike_message.TypeIDi:
			initiatorID = ikePayload.(*ike_message.IdentificationInitiator)
		case ike_message.TypeCERTreq:
			certificateRequest = ikePayload.(*ike_message.CertificateRequest)
		case ike_message.TypeCERT:
			certificate = ikePayload.(*ike_message.Certificate)
		case ike_message.TypeSA:
			securityAssociation = ikePayload.(*ike_message.SecurityAssociation)
		case ike_message.TypeTSi:
			trafficSelectorInitiator = ikePayload.(*ike_message.TrafficSelectorInitiator)
		case ike_message.TypeTSr:
			trafficSelectorResponder = ikePayload.(*ike_message.TrafficSelectorResponder)
		case ike_message.TypeEAP:
			eap = ikePayload.(*ike_message.EAP)
		case ike_message.TypeAUTH:
			authentication = ikePayload.(*ike_message.Authentication)
		case ike_message.TypeCP:
			configuration = ikePayload.(*ike_message.Configuration)
		default:
			logger.IKELog.Warnf(
				"get IKE payload (type %d) in IKE_AUTH message, this payload will not be handled by IKE handler",
				ikePayload.Type())
		}
	}

	// NOTE: tune it
	transformPseudorandomFunction := ikeSecurityAssociation.PseudorandomFunction
	ikeSecurityAssociation.InitiatorMessageID = message.MessageID

	switch ikeSecurityAssociation.State {
	case PreSignalling:
		if initiatorID == nil {
			logger.IKELog.Errorln("the initiator identification field is nil")
			// TODO: send error message to UE
			return
		}
		logger.IKELog.Debugln("encoding initiator for later IKE authentication")
		ikeSecurityAssociation.InitiatorID = initiatorID

		// Record maced identification for authentication
		idPayload := ike_message.IKEPayloadContainer{
			initiatorID,
		}
		idPayloadData, err := idPayload.Encode()
		if err != nil {
			logger.IKELog.Errorf("encoding ID payload message failed: %+v", err)
			return
		}
		pseudorandomFunction, ok := NewPseudorandomFunction(ikeSecurityAssociation.SK_pi,
			transformPseudorandomFunction.TransformID)
		if !ok {
			logger.IKELog.Errorln("get an unsupported pseudorandom funcion. This may imply an unsupported transform is chosen")
			return
		}
		if _, err := pseudorandomFunction.Write(idPayloadData[4:]); err != nil {
			logger.IKELog.Errorf("pseudorandom function write error: %+v", err)
			return
		}
		ikeSecurityAssociation.InitiatorSignedOctets = append(ikeSecurityAssociation.InitiatorSignedOctets, pseudorandomFunction.Sum(nil)...)

		// Certificate request and prepare coresponding certificate
		// RFC 7296 section 3.7:
		// The Certificate Request payload is processed by inspecting the
		// Cert Encoding field to determine whether the processor has any
		// certificates of this type.  If so, the Certification Authority field
		// is inspected to determine if the processor has any certificates that
		// can be validated up to one of the specified certification
		// authorities.  This can be a chain of certificates.
		if certificateRequest != nil {
			logger.IKELog.Infoln("UE request N3IWF certificate")
			if CompareRootCertificate(certificateRequest.CertificateEncoding, certificateRequest.CertificationAuthority) {
				// TODO: Complete N3IWF Certificate/Certificate Authority related procedure
				logger.IKELog.Infoln("certificate Request sent from UE matches N3IWF CA")
			}
		}

		if certificate != nil {
			logger.IKELog.Infoln("UE send its certficate")
			ikeSecurityAssociation.InitiatorCertificate = certificate
		}

		if securityAssociation == nil {
			logger.IKELog.Errorln("the security association field is nil")
			// TODO: send error message to UE
			return
		}
		logger.IKELog.Debugln("parsing security association")
		responseSecurityAssociation := new(ike_message.SecurityAssociation)

		for _, proposal := range securityAssociation.Proposals {
			var encryptionAlgorithmTransform *ike_message.Transform = nil
			var integrityAlgorithmTransform *ike_message.Transform = nil
			var diffieHellmanGroupTransform *ike_message.Transform = nil
			var extendedSequenceNumbersTransform *ike_message.Transform = nil

			if len(proposal.SPI) != 4 {
				continue // The SPI of ESP must be 32-bit
			}

			if len(proposal.EncryptionAlgorithm) > 0 {
				for _, transform := range proposal.EncryptionAlgorithm {
					if is_Kernel_Supported(ike_message.TypeEncryptionAlgorithm, transform.TransformID,
						transform.AttributePresent, transform.AttributeValue) {
						encryptionAlgorithmTransform = transform
						break
					}
				}
				if encryptionAlgorithmTransform == nil {
					continue
				}
			} else {
				continue // mandatory
			}
			if len(proposal.PseudorandomFunction) > 0 {
				continue // Pseudorandom function is not used by ESP
			}
			if len(proposal.IntegrityAlgorithm) > 0 {
				for _, transform := range proposal.IntegrityAlgorithm {
					if is_Kernel_Supported(ike_message.TypeIntegrityAlgorithm, transform.TransformID,
						transform.AttributePresent, transform.AttributeValue) {
						integrityAlgorithmTransform = transform
						break
					}
				}
				if integrityAlgorithmTransform == nil {
					continue
				}
			} // Optional
			if len(proposal.DiffieHellmanGroup) > 0 {
				for _, transform := range proposal.DiffieHellmanGroup {
					if is_Kernel_Supported(ike_message.TypeDiffieHellmanGroup, transform.TransformID,
						transform.AttributePresent, transform.AttributeValue) {
						diffieHellmanGroupTransform = transform
						break
					}
				}
				if diffieHellmanGroupTransform == nil {
					continue
				}
			} // Optional
			if len(proposal.ExtendedSequenceNumbers) > 0 {
				for _, transform := range proposal.ExtendedSequenceNumbers {
					if is_Kernel_Supported(ike_message.TypeExtendedSequenceNumbers, transform.TransformID,
						transform.AttributePresent, transform.AttributeValue) {
						extendedSequenceNumbersTransform = transform
						break
					}
				}
				if extendedSequenceNumbersTransform == nil {
					continue
				}
			} else {
				continue // Mandatory
			}

			chosenProposal := responseSecurityAssociation.Proposals.BuildProposal(
				proposal.ProposalNumber, proposal.ProtocolID, proposal.SPI)
			chosenProposal.EncryptionAlgorithm = append(chosenProposal.EncryptionAlgorithm, encryptionAlgorithmTransform)
			chosenProposal.ExtendedSequenceNumbers = append(
				chosenProposal.ExtendedSequenceNumbers, extendedSequenceNumbersTransform)
			if integrityAlgorithmTransform != nil {
				chosenProposal.IntegrityAlgorithm = append(chosenProposal.IntegrityAlgorithm, integrityAlgorithmTransform)
			}
			if diffieHellmanGroupTransform != nil {
				chosenProposal.DiffieHellmanGroup = append(chosenProposal.DiffieHellmanGroup, diffieHellmanGroupTransform)
			}

			break
		}

		if len(responseSecurityAssociation.Proposals) == 0 {
			logger.IKELog.Warnln("no proposal chosen")
			// Respond NO_PROPOSAL_CHOSEN to UE
			// Build IKE message
			responseIKEMessage.BuildIKEHeader(message.InitiatorSPI, message.ResponderSPI,
				ike_message.IKE_AUTH, ike_message.ResponseBitCheck, message.MessageID)
			responseIKEMessage.Payloads.Reset()

			// Build response
			responseIKEPayload.Reset()

			// Notification
			responseIKEPayload.BuildNotification(ike_message.TypeNone, ike_message.NO_PROPOSAL_CHOSEN, nil, nil)

			if err := EncryptProcedure(ikeSecurityAssociation, responseIKEPayload, responseIKEMessage); err != nil {
				logger.IKELog.Errorf("encrypting IKE message failed: %+v", err)
				return
			}

			// Send IKE message to UE
			SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)

			return
		}

		ikeSecurityAssociation.IKEAuthResponseSA = responseSecurityAssociation

		if trafficSelectorInitiator == nil {
			logger.IKELog.Errorln("the initiator traffic selector field is nil")
			// TODO: send error message to UE
			return
		}
		logger.IKELog.Debugln("received traffic selector initiator from UE")
		ikeSecurityAssociation.TrafficSelectorInitiator = trafficSelectorInitiator

		if trafficSelectorResponder == nil {
			logger.IKELog.Errorln("the responder traffic selector field is nil")
			// TODO: send error message to UE
			return
		}
		logger.IKELog.Debugln("received traffic selector responder from UE")
		ikeSecurityAssociation.TrafficSelectorResponder = trafficSelectorResponder

		// Build response IKE message
		responseIKEMessage.BuildIKEHeader(message.InitiatorSPI, message.ResponderSPI,
			ike_message.IKE_AUTH, ike_message.ResponseBitCheck, message.MessageID)
		responseIKEMessage.Payloads.Reset()

		// Identification
		responseIKEPayload.BuildIdentificationResponder(ike_message.ID_FQDN, []byte(n3iwfSelf.Fqdn))

		// Certificate
		responseIKEPayload.BuildCertificate(ike_message.X509CertificateSignature, n3iwfSelf.N3iwfCertificate)

		// Authentication Data
		logger.IKELog.Debugf("local authentication data:\n%s", hex.Dump(ikeSecurityAssociation.ResponderSignedOctets))
		sha1HashFunction := sha1.New()
		if _, err := sha1HashFunction.Write(ikeSecurityAssociation.ResponderSignedOctets); err != nil {
			logger.IKELog.Errorf("hash function write error: %+v", err)
			return
		}

		signedAuth, err := rsa.SignPKCS1v15(rand.Reader, n3iwfSelf.N3iwfPrivateKey, crypto.SHA1, sha1HashFunction.Sum(nil))
		if err != nil {
			logger.IKELog.Errorf("sign authentication data failed: %+v", err)
		}

		responseIKEPayload.BuildAuthentication(ike_message.RSADigitalSignature, signedAuth)

		// EAP expanded 5G-Start
		var identifier uint8
		for {
			identifier, err = GenerateRandomUint8()
			if err != nil {
				logger.IKELog.Errorf("random number failed: %+v", err)
				return
			}
			if identifier != ikeSecurityAssociation.LastEAPIdentifier {
				ikeSecurityAssociation.LastEAPIdentifier = identifier
				break
			}
		}
		responseIKEPayload.BuildEAP5GStart(identifier)

		if err := EncryptProcedure(ikeSecurityAssociation, responseIKEPayload, responseIKEMessage); err != nil {
			logger.IKELog.Errorf("encrypting IKE message failed: %+v", err)
			return
		}

		// Shift state
		ikeSecurityAssociation.State++

		// Send IKE message to UE
		SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)

	case EAPSignalling:
		// If success, N3IWF will send an UPLinkNASTransport to AMF
		if eap == nil {
			logger.IKELog.Errorln("EAP is nil")
			return
		}
		if eap.Code != ike_message.EAPCodeResponse {
			logger.IKELog.Errorln("received an EAP payload with code other than response. Drop the payload")
			return
		}
		if eap.Identifier != ikeSecurityAssociation.LastEAPIdentifier {
			logger.IKELog.Errorln("received an EAP payload with unmatched identifier. Drop the payload")
			return
		}

		eapTypeData := eap.EAPTypeData[0]
		var eapExpanded *ike_message.EAPExpanded

		switch eapTypeData.Type() {
		// TODO: handle
		// case ike_message.EAPTypeIdentity:
		// case ike_message.EAPTypeNotification:
		// case ike_message.EAPTypeNak:
		case ike_message.EAPTypeExpanded:
			eapExpanded = eapTypeData.(*ike_message.EAPExpanded)
		default:
			logger.IKELog.Errorf("received EAP packet with type other than EAP expanded type: %d", eapTypeData.Type())
			return
		}

		if eapExpanded.VendorID != ike_message.VendorID3GPP {
			logger.IKELog.Errorln("the peer sent EAP expended packet with wrong vendor ID. Drop the packet")
			return
		}
		if eapExpanded.VendorType != ike_message.VendorTypeEAP5G {
			logger.IKELog.Errorln("the peer sent EAP expanded packet with wrong vendor type. Drop the packet")
			return
		}

		eap5GMessageID := eapExpanded.VendorData[0]
		logger.IKELog.Debugf("EAP5G MessageID: %+v", eap5GMessageID)

		if eap5GMessageID == ike_message.EAP5GType5GStop {
			// Send EAP failure
			// Build IKE message
			responseIKEMessage.BuildIKEHeader(message.InitiatorSPI, message.ResponderSPI,
				ike_message.IKE_AUTH, ike_message.ResponseBitCheck, message.MessageID)
			responseIKEMessage.Payloads.Reset()

			// EAP
			identifier, err := GenerateRandomUint8()
			if err != nil {
				logger.IKELog.Errorf("generate random uint8 failed: %+v", err)
				return
			}
			responseIKEPayload.BuildEAPFailure(identifier)

			if err := EncryptProcedure(ikeSecurityAssociation, responseIKEPayload, responseIKEMessage); err != nil {
				logger.IKELog.Errorf("encrypting IKE message failed: %+v", err)
				return
			}

			// Send IKE message to UE
			SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)
			return
		}

		var ranNgapId int64
		ranNgapId, ok = n3iwfSelf.NgapIdLoad(ikeSecurityAssociation.LocalSPI)
		if !ok {
			ranNgapId = 0
		}

		n3iwfSelf.NgapServer.RcvEventCh <- context.NewUnmarshalEAP5GDataEvt(
			ikeSecurityAssociation.LocalSPI,
			eapExpanded.VendorData,
			ikeSecurityAssociation.IkeUE != nil,
			ranNgapId,
		)

		ikeSecurityAssociation.IKEConnection = &context.UDPSocketInfo{
			Conn:      udpConn,
			N3IWFAddr: n3iwfAddr,
			UEAddr:    ueAddr,
		}

		ikeSecurityAssociation.InitiatorMessageID = message.MessageID

	case PostSignalling:
		// Load needed information
		ikeUE := ikeSecurityAssociation.IkeUE

		// Prepare pseudorandom function for calculating/verifying authentication data
		pseudorandomFunction, ok := NewPseudorandomFunction(ikeUE.Kn3iwf, transformPseudorandomFunction.TransformID)
		if !ok {
			logger.IKELog.Error("get an unsupported pseudorandom funcion. This may imply an unsupported transform is chosen")
			return
		}
		if _, err := pseudorandomFunction.Write([]byte("Key Pad for IKEv2")); err != nil {
			logger.IKELog.Errorf("pseudorandom function write error: %+v", err)
			return
		}
		secret := pseudorandomFunction.Sum(nil)
		pseudorandomFunction, ok = NewPseudorandomFunction(secret, transformPseudorandomFunction.TransformID)
		if !ok {
			logger.IKELog.Error("get an unsupported pseudorandom funcion. This may imply an unsupported transform is chosen")
			return
		}

		if authentication != nil {
			// Verifying remote AUTH
			pseudorandomFunction.Reset()
			if _, err := pseudorandomFunction.Write(ikeSecurityAssociation.InitiatorSignedOctets); err != nil {
				logger.IKELog.Errorf("pseudorandom function write error: %+v", err)
				return
			}
			expectedAuthenticationData := pseudorandomFunction.Sum(nil)

			logger.IKELog.Debugf("expected Authentication Data: %s", hex.Dump(expectedAuthenticationData))
			if !bytes.Equal(authentication.AuthenticationData, expectedAuthenticationData) {
				logger.IKELog.Warnln("peer authentication failed")
				// Inform UE the authentication has failed
				// Build IKE message
				responseIKEMessage.BuildIKEHeader(message.InitiatorSPI, message.ResponderSPI,
					ike_message.IKE_AUTH, ike_message.ResponseBitCheck, message.MessageID)
				responseIKEMessage.Payloads.Reset()
				// Notification
				responseIKEPayload.BuildNotification(ike_message.TypeNone, ike_message.AUTHENTICATION_FAILED, nil, nil)
				if err := EncryptProcedure(ikeSecurityAssociation, responseIKEPayload, responseIKEMessage); err != nil {
					logger.IKELog.Errorf("encrypting IKE message failed: %+v", err)
					return
				}

				// Send IKE message to UE
				SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)
				return
			} else {
				logger.IKELog.Debugln("peer authentication success")
			}
		} else {
			logger.IKELog.Warnln("peer authentication failed")
			// Inform UE the authentication has failed
			// Build IKE message
			responseIKEMessage.BuildIKEHeader(message.InitiatorSPI, message.ResponderSPI,
				ike_message.IKE_AUTH, ike_message.ResponseBitCheck, message.MessageID)
			responseIKEMessage.Payloads.Reset()

			// Notification
			responseIKEPayload.BuildNotification(ike_message.TypeNone, ike_message.AUTHENTICATION_FAILED, nil, nil)

			if err := EncryptProcedure(ikeSecurityAssociation, responseIKEPayload, responseIKEMessage); err != nil {
				logger.IKELog.Errorf("encrypting IKE message failed: %+v", err)
				return
			}

			// Send IKE message to UE
			SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)
			return
		}

		// Parse configuration request to get if the UE has requested internal address,
		// and prepare configuration payload to UE
		var addrRequest bool = false

		if configuration != nil {
			logger.IKELog.Debugf("received configuration payload with type: %d", configuration.ConfigurationType)

			var attribute *ike_message.IndividualConfigurationAttribute
			for _, attribute = range configuration.ConfigurationAttribute {
				switch attribute.Type {
				case ike_message.INTERNAL_IP4_ADDRESS:
					addrRequest = true
					if len(attribute.Value) != 0 {
						logger.IKELog.Debugf("got client requested address: %d.%d.%d.%d",
							attribute.Value[0], attribute.Value[1], attribute.Value[2], attribute.Value[3])
					}
				default:
					logger.IKELog.Warnf("receive other type of configuration request: %d", attribute.Type)
				}
			}
		} else {
			logger.IKELog.Warnln("configuration is nil. UE did not sent any configuration request")
		}

		// Build response IKE message
		responseIKEMessage.BuildIKEHeader(message.InitiatorSPI, message.ResponderSPI,
			ike_message.IKE_AUTH, ike_message.ResponseBitCheck, message.MessageID)
		responseIKEMessage.Payloads.Reset()

		// Calculate local AUTH
		pseudorandomFunction.Reset()
		if _, err := pseudorandomFunction.Write(ikeSecurityAssociation.ResponderSignedOctets); err != nil {
			logger.IKELog.Errorf("pseudorandom function write error: %+v", err)
			return
		}

		// Authentication
		responseIKEPayload.BuildAuthentication(
			ike_message.SharedKeyMesageIntegrityCode, pseudorandomFunction.Sum(nil))

		// Prepare configuration payload and traffic selector payload for initiator and responder
		var ueIPAddr, n3iwfIPAddr net.IP
		if !addrRequest {
			logger.IKELog.Errorln("UE did not send any configuration request for its IP address")
			return
		}
		// IP addresses (IPSec)
		ueIPAddr = n3iwfSelf.NewInternalUEIPAddr(ikeUE).To4()
		n3iwfIPAddr = net.ParseIP(n3iwfSelf.IpSecGatewayAddress).To4()

		responseConfiguration := responseIKEPayload.BuildConfiguration(ike_message.CFG_REPLY)
		responseConfiguration.ConfigurationAttribute.BuildConfigurationAttribute(ike_message.INTERNAL_IP4_ADDRESS, ueIPAddr)
		responseConfiguration.ConfigurationAttribute.BuildConfigurationAttribute(
			ike_message.INTERNAL_IP4_NETMASK, n3iwfSelf.Subnet.Mask)

		ikeUE.IPSecInnerIP = ueIPAddr
		if ipsecInnerIPAddr, err := net.ResolveIPAddr("ip", ueIPAddr.String()); err != nil {
			logger.IKELog.Errorf("resolve UE inner IP address failed: %+v", err)
			return
		} else {
			ikeUE.IPSecInnerIPAddr = ipsecInnerIPAddr
		}
		logger.IKELog.Debugf("ueIPAddr: %+v", ueIPAddr)

		// Security Association
		responseIKEPayload = append(responseIKEPayload, ikeSecurityAssociation.IKEAuthResponseSA)

		// Traffic Selectors initiator/responder
		responseTrafficSelectorInitiator := responseIKEPayload.BuildTrafficSelectorInitiator()
		responseTrafficSelectorInitiator.TrafficSelectors.BuildIndividualTrafficSelector(
			ike_message.TS_IPV4_ADDR_RANGE, ike_message.IPProtocolAll, 0, 65535, ueIPAddr.To4(), ueIPAddr.To4())
		responseTrafficSelectorResponder := responseIKEPayload.BuildTrafficSelectorResponder()
		responseTrafficSelectorResponder.TrafficSelectors.BuildIndividualTrafficSelector(
			ike_message.TS_IPV4_ADDR_RANGE, ike_message.IPProtocolAll, 0, 65535, n3iwfIPAddr.To4(), n3iwfIPAddr.To4())

		// Record traffic selector to IKE security association
		ikeSecurityAssociation.TrafficSelectorInitiator = responseTrafficSelectorInitiator
		ikeSecurityAssociation.TrafficSelectorResponder = responseTrafficSelectorResponder

		// Get data needed by xfrm

		// Allocate N3IWF inbound SPI
		var inboundSPI uint32
		inboundSPIByte := make([]byte, 4)
		for {
			randomUint64 := GenerateRandomNumber().Uint64()
			// check if the inbound SPI havn't been allocated by N3IWF
			if _, ok := n3iwfSelf.ChildSA.Load(uint32(randomUint64)); !ok {
				inboundSPI = uint32(randomUint64)
				break
			}
		}
		binary.BigEndian.PutUint32(inboundSPIByte, inboundSPI)

		outboundSPI := binary.BigEndian.Uint32(ikeSecurityAssociation.IKEAuthResponseSA.Proposals[0].SPI)
		logger.IKELog.Debugf("inbound SPI: %+v, outbound SPI: %+v", inboundSPI, outboundSPI)

		// SPI field of IKEAuthResponseSA is used to save outbound SPI temporarily.
		// After N3IWF produced its inbound SPI, the field will be overwritten with the SPI.
		ikeSecurityAssociation.IKEAuthResponseSA.Proposals[0].SPI = inboundSPIByte

		// Consider 0x01 as the speicified index for IKE_AUTH exchange
		ikeUE.CreateHalfChildSA(0x01, inboundSPI, -1)
		childSecurityAssociationContext, err := ikeUE.CompleteChildSA(0x01, outboundSPI, ikeSecurityAssociation.IKEAuthResponseSA)
		if err != nil {
			logger.IKELog.Errorf("create child security association context failed: %+v", err)
			return
		}
		err = parseIPAddressInformationToChildSecurityAssociation(childSecurityAssociationContext, ueAddr.IP,
			ikeSecurityAssociation.TrafficSelectorResponder.TrafficSelectors[0],
			ikeSecurityAssociation.TrafficSelectorInitiator.TrafficSelectors[0])
		if err != nil {
			logger.IKELog.Errorf("parse IP address to child security association failed: %+v", err)
			return
		}
		// Select TCP traffic
		childSecurityAssociationContext.SelectedIPProtocol = unix.IPPROTO_TCP

		if errGen := GenerateKeyForChildSA(ikeSecurityAssociation, childSecurityAssociationContext); errGen != nil {
			logger.IKELog.Errorf("generate key for child SA failed: %+v", errGen)
			return
		}
		// NAT-T concern
		if ikeSecurityAssociation.UEIsBehindNAT || ikeSecurityAssociation.N3IWFIsBehindNAT {
			childSecurityAssociationContext.EnableEncapsulate = true
			childSecurityAssociationContext.N3IWFPort = n3iwfAddr.Port
			childSecurityAssociationContext.NATPort = ueAddr.Port
		}

		// Notification(NAS_IP_ADDRESS)
		responseIKEPayload.BuildNotifyNAS_IP4_ADDRESS(n3iwfSelf.IpSecGatewayAddress)

		// Notification(NSA_TCP_PORT)
		responseIKEPayload.BuildNotifyNAS_TCP_PORT(n3iwfSelf.TcpPort)

		if errEncrypt := EncryptProcedure(ikeSecurityAssociation, responseIKEPayload, responseIKEMessage); errEncrypt != nil {
			logger.IKELog.Errorf("encrypting IKE message failed: %+v", errEncrypt)
			return
		}

		// Apply XFRM rules
		// IPsec for CP always use default XFRM interface
		if err = xfrm.ApplyXFRMRule(false, n3iwfSelf.XfrmInterfaceId, childSecurityAssociationContext); err != nil {
			logger.IKELog.Errorf("applying XFRM rules failed: %+v", err)
			return
		}

		// Send IKE message to UE
		SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)

		ranNgapId, ok := n3iwfSelf.NgapIdLoad(ikeUE.N3IWFIKESecurityAssociation.LocalSPI)
		if !ok {
			logger.IKELog.Errorf("cannot get RanNgapId from SPI: %+v", ikeUE.N3IWFIKESecurityAssociation.LocalSPI)
			return
		}

		ikeSecurityAssociation.State++

		// After this, N3IWF will forward NAS with Child SA (IPSec SA)
		n3iwfSelf.NgapServer.RcvEventCh <- context.NewStartTCPSignalNASMsgEvt(
			ranNgapId,
		)

		// Get TempPDUSessionSetupData from NGAP to setup PDU session if needed
		n3iwfSelf.NgapServer.RcvEventCh <- context.NewGetNGAPContextEvt(
			ranNgapId, []int64{context.CxtTempPDUSessionSetupData},
		)
	}
}

func HandleCREATECHILDSA(udpConn *net.UDPConn, n3iwfAddr, ueAddr *net.UDPAddr, message *ike_message.IKEMessage) {
	logger.IKELog.Debugln("handle CREATE_CHILD_SA")

	var encryptedPayload *ike_message.Encrypted

	n3iwfSelf := context.N3IWFSelf()

	responseIKEMessage := new(ike_message.IKEMessage)

	if message == nil {
		logger.IKELog.Errorln("IKE Message is nil")
		return
	}

	// parse IKE header and setup IKE context
	// check major version
	majorVersion := ((message.Version & 0xf0) >> 4)
	if majorVersion > 2 {
		logger.IKELog.Warnln("received an IKE message with higher major version")
		// send INFORMATIONAL type message with INVALID_MAJOR_VERSION Notify payload ( OUTSIDE IKE SA )
		responseIKEMessage.BuildIKEHeader(message.InitiatorSPI, message.ResponderSPI,
			ike_message.INFORMATIONAL, ike_message.ResponseBitCheck, message.MessageID)
		responseIKEMessage.Payloads.Reset()
		responseIKEMessage.Payloads.BuildNotification(ike_message.TypeNone, ike_message.INVALID_MAJOR_VERSION, nil, nil)

		SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)

		return
	}

	// Find corresponding IKE security association
	responderSPI := message.ResponderSPI

	logger.IKELog.Debugf("create child SA responderSPI: %+v", responderSPI)
	ikeSecurityAssociation, ok := n3iwfSelf.IKESALoad(responderSPI)
	if !ok {
		logger.IKELog.Warnln("unrecognized SPI")
		// send INFORMATIONAL type message with INVALID_IKE_SPI Notify payload ( OUTSIDE IKE SA )
		responseIKEMessage.BuildIKEHeader(0, message.ResponderSPI, ike_message.INFORMATIONAL,
			ike_message.ResponseBitCheck, message.MessageID)
		responseIKEMessage.Payloads.Reset()
		responseIKEMessage.Payloads.BuildNotification(ike_message.TypeNone, ike_message.INVALID_IKE_SPI, nil, nil)

		SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)

		return
	}

	for _, ikePayload := range message.Payloads {
		switch ikePayload.Type() {
		case ike_message.TypeSK:
			encryptedPayload = ikePayload.(*ike_message.Encrypted)
		default:
			logger.IKELog.Warnf(
				"get IKE payload (type %d) in CREATE_CHILD_SA message, this payload will not be handled by IKE handler",
				ikePayload.Type())
		}
	}

	decryptedIKEPayload, err := DecryptProcedure(ikeSecurityAssociation, message, encryptedPayload)
	if err != nil {
		logger.IKELog.Errorf("decrypt IKE message failed: %+v", err)
		return
	}

	// Parse payloads
	var securityAssociation *ike_message.SecurityAssociation
	var nonce *ike_message.Nonce
	var trafficSelectorInitiator *ike_message.TrafficSelectorInitiator
	var trafficSelectorResponder *ike_message.TrafficSelectorResponder

	for _, ikePayload := range decryptedIKEPayload {
		switch ikePayload.Type() {
		case ike_message.TypeSA:
			securityAssociation = ikePayload.(*ike_message.SecurityAssociation)
		case ike_message.TypeNiNr:
			nonce = ikePayload.(*ike_message.Nonce)
		case ike_message.TypeTSi:
			trafficSelectorInitiator = ikePayload.(*ike_message.TrafficSelectorInitiator)
		case ike_message.TypeTSr:
			trafficSelectorResponder = ikePayload.(*ike_message.TrafficSelectorResponder)
		default:
			logger.IKELog.Warnf(
				"get IKE payload (type %d) in CREATE_CHILD_SA message, this payload will not be handled by IKE handler",
				ikePayload.Type())
		}
	}

	// Check received message
	if securityAssociation == nil {
		logger.IKELog.Errorln("the security association field is nil")
		return
	}

	if trafficSelectorInitiator == nil {
		logger.IKELog.Errorln("the traffic selector initiator field is nil")
		return
	}

	if trafficSelectorResponder == nil {
		logger.IKELog.Errorln("the traffic selector responder field is nil")
		return
	}

	// Nonce
	if nonce == nil {
		logger.IKELog.Errorln("the nonce field is nil")
		// TODO: send error message to UE
		return
	}
	ikeSecurityAssociation.ConcatenatedNonce = append(ikeSecurityAssociation.ConcatenatedNonce, nonce.NonceData...)

	ikeSecurityAssociation.TemporaryIkeMsg = &context.IkeMsgTemporaryData{
		SecurityAssociation:      securityAssociation,
		TrafficSelectorInitiator: trafficSelectorInitiator,
		TrafficSelectorResponder: trafficSelectorResponder,
	}

	ranNgapId, ok := n3iwfSelf.NgapIdLoad(ikeSecurityAssociation.LocalSPI)
	if !ok {
		logger.IKELog.Errorf("cannot get RanNgapID from SPI: %+v", ikeSecurityAssociation.LocalSPI)
		return
	}

	ngapCxtReqNumlist := []int64{context.CxtTempPDUSessionSetupData}

	n3iwfSelf.NgapServer.RcvEventCh <- context.NewGetNGAPContextEvt(ranNgapId,
		ngapCxtReqNumlist)
}

func continueCreateChildSA(ikeSecurityAssociation *context.IKESecurityAssociation,
	temporaryPDUSessionSetupData *context.PDUSessionSetupTemporaryData,
) {
	n3iwfSelf := context.N3IWFSelf()

	// UE context
	ikeUe := ikeSecurityAssociation.IkeUE
	if ikeUe == nil {
		logger.IKELog.Errorln("UE context is nil")
		return
	}

	// PDU session information
	if temporaryPDUSessionSetupData == nil {
		logger.IKELog.Errorln("no PDU session information")
		return
	}

	if len(temporaryPDUSessionSetupData.UnactivatedPDUSession) == 0 {
		logger.IKELog.Errorln("no unactivated PDU session information")
		return
	}

	temporaryIkeMsg := ikeSecurityAssociation.TemporaryIkeMsg
	ikeConnection := ikeSecurityAssociation.IKEConnection

	// Get xfrm needed data
	// As specified in RFC 7296, ESP negotiate two child security association (pair) in one exchange
	// Message ID is used to be a index to pair two SPI in serveral IKE messages.
	outboundSPI := binary.BigEndian.Uint32(temporaryIkeMsg.SecurityAssociation.Proposals[0].SPI)
	childSecurityAssociationContext, err := ikeUe.CompleteChildSA(
		ikeSecurityAssociation.ResponderMessageID, outboundSPI, temporaryIkeMsg.SecurityAssociation)
	if err != nil {
		logger.IKELog.Errorf("create child security association context failed: %+v", err)
		return
	}

	// Build TSi if there is no one in the response
	if len(temporaryIkeMsg.TrafficSelectorInitiator.TrafficSelectors) == 0 {
		logger.IKELog.Warnln("there is no TSi in CREATE_CHILD_SA response")
		n3iwfIPAddr := net.ParseIP(n3iwfSelf.IpSecGatewayAddress)
		temporaryIkeMsg.TrafficSelectorInitiator.TrafficSelectors.BuildIndividualTrafficSelector(
			ike_message.TS_IPV4_ADDR_RANGE, ike_message.IPProtocolAll,
			0, 65535, n3iwfIPAddr, n3iwfIPAddr)
	}

	// Build TSr if there is no one in the response
	if len(temporaryIkeMsg.TrafficSelectorResponder.TrafficSelectors) == 0 {
		logger.IKELog.Warnln("there is no TSr in CREATE_CHILD_SA response")
		ueIPAddr := ikeUe.IPSecInnerIP
		temporaryIkeMsg.TrafficSelectorResponder.TrafficSelectors.BuildIndividualTrafficSelector(
			ike_message.TS_IPV4_ADDR_RANGE, ike_message.IPProtocolAll,
			0, 65535, ueIPAddr, ueIPAddr)
	}

	err = parseIPAddressInformationToChildSecurityAssociation(childSecurityAssociationContext,
		ikeConnection.UEAddr.IP,
		temporaryIkeMsg.TrafficSelectorInitiator.TrafficSelectors[0],
		temporaryIkeMsg.TrafficSelectorResponder.TrafficSelectors[0])
	if err != nil {
		logger.IKELog.Errorf("parse IP address to child security association failed: %+v", err)
		return
	}
	// Select GRE traffic
	childSecurityAssociationContext.SelectedIPProtocol = unix.IPPROTO_GRE

	if errGen := GenerateKeyForChildSA(ikeSecurityAssociation, childSecurityAssociationContext); errGen != nil {
		logger.IKELog.Errorf("generate key for child SA failed: %+v", errGen)
		return
	}
	// NAT-T concern
	if ikeSecurityAssociation.UEIsBehindNAT || ikeSecurityAssociation.N3IWFIsBehindNAT {
		childSecurityAssociationContext.EnableEncapsulate = true
		childSecurityAssociationContext.N3IWFPort = ikeConnection.N3IWFAddr.Port
		childSecurityAssociationContext.NATPort = ikeConnection.UEAddr.Port
	}

	newXfrmiId := n3iwfSelf.XfrmInterfaceId

	// The additional PDU session will be separated from default xfrm interface
	// to avoid SPD entry collision
	if ikeUe.PduSessionListLen > 1 {
		// Setup XFRM interface for ipsec
		var linkIPSec netlink.Link
		n3iwfIPAddr := net.ParseIP(n3iwfSelf.IpSecGatewayAddress).To4()
		n3iwfIPAddrAndSubnet := net.IPNet{IP: n3iwfIPAddr, Mask: n3iwfSelf.Subnet.Mask}
		newXfrmiId += n3iwfSelf.XfrmInterfaceId + n3iwfSelf.XfrmIfaceIdOffsetForUP
		newXfrmiName := fmt.Sprintf("%s-%d", n3iwfSelf.XfrmInterfaceName, newXfrmiId)

		if linkIPSec, err = xfrm.SetupIPsecXfrmi(newXfrmiName, n3iwfSelf.XfrmParentIfaceName,
			newXfrmiId, n3iwfIPAddrAndSubnet); err != nil {
			logger.IKELog.Errorf("setup XFRM interface %s fail: %+v", newXfrmiName, err)
			return
		}

		n3iwfSelf.XfrmIfaces.LoadOrStore(newXfrmiId, linkIPSec)
		childSecurityAssociationContext.XfrmIface = linkIPSec
		n3iwfSelf.XfrmIfaceIdOffsetForUP++
	} else {
		if linkIPSec, ok := n3iwfSelf.XfrmIfaces.Load(newXfrmiId); ok {
			childSecurityAssociationContext.XfrmIface = linkIPSec.(netlink.Link)
		} else {
			logger.IKELog.Warnf("cannot find the XFRM interface with if_id: %d", newXfrmiId)
			return
		}
	}

	// Apply XFRM rules
	if err = xfrm.ApplyXFRMRule(true, newXfrmiId, childSecurityAssociationContext); err != nil {
		logger.IKELog.Errorf("applying XFRM rules failed: %+v", err)
		return
	} else {
		ranNgapId, ok := n3iwfSelf.NgapIdLoad(ikeSecurityAssociation.LocalSPI)
		if !ok {
			logger.IKELog.Errorf("cannot get RanNgapId from SPI: %+v", ikeSecurityAssociation.LocalSPI)
			return
		}
		// Forward PDU Seesion Establishment Accept to UE
		n3iwfSelf.NgapServer.RcvEventCh <- context.NewSendNASMsgEvt(
			ranNgapId,
		)
	}

	temporaryPDUSessionSetupData.FailedErrStr = append(temporaryPDUSessionSetupData.FailedErrStr, context.ErrNil)

	ikeSecurityAssociation.ResponderMessageID++

	// If needed, setup another PDU session
	CreatePDUSessionChildSA(ikeUe, temporaryPDUSessionSetupData)
}

func HandleInformational(udpConn *net.UDPConn, n3iwfAddr, ueAddr *net.UDPAddr, message *ike_message.IKEMessage) {
	logger.IKELog.Debugln("handle Informational")

	if message == nil {
		logger.IKELog.Errorln("IKE Message is nil")
		return
	}

	n3iwfSelf := context.N3IWFSelf()
	responseIKEMessage := new(ike_message.IKEMessage)
	responderSPI := message.ResponderSPI
	ikeSecurityAssociation, ok := n3iwfSelf.IKESALoad(responderSPI)
	var encryptedPayload *ike_message.Encrypted

	if !ok {
		logger.IKELog.Warnln("unrecognized SPI")
		// send INFORMATIONAL type message with INVALID_IKE_SPI Notify payload ( OUTSIDE IKE SA )
		responseIKEMessage.BuildIKEHeader(0, message.ResponderSPI, ike_message.INFORMATIONAL,
			ike_message.ResponseBitCheck, message.MessageID)
		responseIKEMessage.Payloads.Reset()
		responseIKEMessage.Payloads.BuildNotification(ike_message.TypeNone, ike_message.INVALID_IKE_SPI, nil, nil)

		SendIKEMessageToUE(udpConn, n3iwfAddr, ueAddr, responseIKEMessage)

		return
	}

	for _, ikePayload := range message.Payloads {
		switch ikePayload.Type() {
		case ike_message.TypeSK:
			encryptedPayload = ikePayload.(*ike_message.Encrypted)
		default:
			logger.IKELog.Warnf(
				"get IKE payload (type %d) in Inoformational message, this payload will not be handled by IKE handler",
				ikePayload.Type())
		}
	}

	decryptedIKEPayload, err := DecryptProcedure(ikeSecurityAssociation, message, encryptedPayload)
	if err != nil {
		logger.IKELog.Errorf("decrypt IKE message failed: %+v", err)
		return
	}

	n3iwfIke := ikeSecurityAssociation.IkeUE

	if n3iwfIke.N3IWFIKESecurityAssociation.DPDReqRetransTimer != nil {
		n3iwfIke.N3IWFIKESecurityAssociation.DPDReqRetransTimer.Stop()
		n3iwfIke.N3IWFIKESecurityAssociation.DPDReqRetransTimer = nil
		atomic.StoreInt32(&n3iwfIke.N3IWFIKESecurityAssociation.CurrentRetryTimes, 0)
	}

	if len(decryptedIKEPayload) == 0 { // Receive DPD message
		return
	}

	for _, ikePayload := range decryptedIKEPayload {
		switch ikePayload.Type() {
		case ike_message.TypeD:
			deletePayload := ikePayload.(*ike_message.Delete)

			ranNgapId, ok := n3iwfSelf.NgapIdLoad(n3iwfIke.N3IWFIKESecurityAssociation.LocalSPI)
			if !ok {
				logger.IKELog.Errorf("cannot get RanNgapId from SPI: %+v", n3iwfIke.N3IWFIKESecurityAssociation.LocalSPI)
				return
			}

			switch deletePayload.ProtocolID {
			case ike_message.TypeIKE: // Check if UE is response to a request that delete the ike SA
				if err := n3iwfIke.Remove(); err != nil {
					logger.IKELog.Errorf("delete IkeUe Context error: %+v", err)
				}
				n3iwfSelf.NgapServer.RcvEventCh <- context.NewSendUEContextReleaseCompleteEvt(
					ranNgapId,
				)
			case ike_message.TypeESP:
				n3iwfSelf.NgapServer.RcvEventCh <- context.NewSendPDUSessionResourceReleaseResEvt(
					ranNgapId,
				)
			}
		default:
			logger.IKELog.Warnf(
				"get IKE payload (type %d) in Inoformational message, this payload will not be handled by IKE handler",
				ikePayload.Type())
		}
	}
	ikeSecurityAssociation.ResponderMessageID++
}

func HandleEvent(ikeEvt context.IkeEvt) {
	logger.IKELog.Debugln("handle IKE event")

	switch ikeEvt.Type() {
	case context.UnmarshalEAP5GDataResponse:
		HandleUnmarshalEAP5GDataResponse(ikeEvt)
	case context.SendEAP5GFailureMsg:
		HandleSendEAP5GFailureMsg(ikeEvt)
	case context.SendEAPSuccessMsg:
		HandleSendEAPSuccessMsg(ikeEvt)
	case context.SendEAPNASMsg:
		HandleSendEAPNASMsg(ikeEvt)
	case context.CreatePDUSession:
		HandleCreatePDUSession(ikeEvt)
	case context.IKEDeleteRequest:
		HandleIKEDeleteRequest(ikeEvt)
	case context.SendChildSADeleteRequest:
		HandleSendChildSADeleteRequest(ikeEvt)
	case context.IKEContextUpdate:
		HandleIKEContextUpdate(ikeEvt)
	case context.GetNGAPContextResponse:
		HandleGetNGAPContextResponse(ikeEvt)
	default:
		logger.IKELog.Errorf("undefined IKE event type: %d", ikeEvt.Type())
		return
	}
}

func HandleUnmarshalEAP5GDataResponse(ikeEvt context.IkeEvt) {
	logger.IKELog.Debugln("handle UnmarshalEAP5GDataResponse event")

	unmarshalEAP5GDataResponseEvt := ikeEvt.(*context.UnmarshalEAP5GDataResponseEvt)
	localSPI := unmarshalEAP5GDataResponseEvt.LocalSPI
	ranUeNgapId := unmarshalEAP5GDataResponseEvt.RanUeNgapId
	nasPDU := unmarshalEAP5GDataResponseEvt.NasPDU

	n3iwfSelf := context.N3IWFSelf()
	ikeSecurityAssociation, _ := n3iwfSelf.IKESALoad(localSPI)

	// Create UE context
	ikeUe := n3iwfSelf.NewN3iwfIkeUe(localSPI)

	// Relative context
	ikeSecurityAssociation.IkeUE = ikeUe
	ikeUe.N3IWFIKESecurityAssociation = ikeSecurityAssociation
	ikeUe.IKEConnection = ikeSecurityAssociation.IKEConnection

	n3iwfSelf.IkeSpiNgapIdMapping(ikeUe.N3IWFIKESecurityAssociation.LocalSPI, ranUeNgapId)

	n3iwfSelf.NgapServer.RcvEventCh <- context.NewSendInitialUEMessageEvt(
		ranUeNgapId,
		ikeSecurityAssociation.IKEConnection.UEAddr.IP.To4().String(),
		ikeSecurityAssociation.IKEConnection.UEAddr.Port,
		nasPDU,
	)
}

func HandleSendEAP5GFailureMsg(ikeEvt context.IkeEvt) {
	logger.IKELog.Warnln("handle SendEAP5GFailureMsg event")

	sendEAP5GFailureMsgEvt := ikeEvt.(*context.SendEAP5GFailureMsgEvt)
	errMsg := sendEAP5GFailureMsgEvt.ErrMsg
	localSPI := sendEAP5GFailureMsgEvt.LocalSPI

	n3iwfSelf := context.N3IWFSelf()
	ikeSecurityAssociation, _ := n3iwfSelf.IKESALoad(localSPI)
	logger.IKELog.Warnf("EAP Failure: %s", errMsg.Error())

	responseIKEMessage := new(ike_message.IKEMessage)
	var responseIKEPayload ike_message.IKEPayloadContainer
	// Send EAP failure
	// Build IKE message
	responseIKEMessage.BuildIKEHeader(ikeSecurityAssociation.RemoteSPI, ikeSecurityAssociation.LocalSPI,
		ike_message.IKE_AUTH, ike_message.ResponseBitCheck, ikeSecurityAssociation.InitiatorMessageID)
	responseIKEMessage.Payloads.Reset()

	// EAP
	identifier, err := GenerateRandomUint8()
	if err != nil {
		logger.IKELog.Errorf("generate random uint8 failed: %+v", err)
		return
	}
	responseIKEPayload.BuildEAPFailure(identifier)

	if err := EncryptProcedure(ikeSecurityAssociation, responseIKEPayload, responseIKEMessage); err != nil {
		logger.IKELog.Errorf("encrypting IKE message failed: %+v", err)
		return
	}

	// Send IKE message to UE
	SendIKEMessageToUE(ikeSecurityAssociation.IKEConnection.Conn,
		ikeSecurityAssociation.IKEConnection.N3IWFAddr, ikeSecurityAssociation.IKEConnection.UEAddr,
		responseIKEMessage)
}

func HandleSendEAPSuccessMsg(ikeEvt context.IkeEvt) {
	logger.IKELog.Debugln("handle SendEAPSuccessMsg event")

	sendEAPSuccessMsgEvt := ikeEvt.(*context.SendEAPSuccessMsgEvt)
	localSPI := sendEAPSuccessMsgEvt.LocalSPI
	kn3iwf := sendEAPSuccessMsgEvt.Kn3iwf

	n3iwfSelf := context.N3IWFSelf()
	ikeSecurityAssociation, _ := n3iwfSelf.IKESALoad(localSPI)

	if kn3iwf != nil {
		ikeSecurityAssociation.IkeUE.Kn3iwf = kn3iwf
	}

	ikeSecurityAssociation.IkeUE.PduSessionListLen = sendEAPSuccessMsgEvt.PduSessionListLen

	responseIKEMessage := new(ike_message.IKEMessage)
	var responseIKEPayload ike_message.IKEPayloadContainer

	// Build IKE message
	responseIKEMessage.BuildIKEHeader(ikeSecurityAssociation.RemoteSPI,
		ikeSecurityAssociation.LocalSPI, ike_message.IKE_AUTH, ike_message.ResponseBitCheck,
		ikeSecurityAssociation.InitiatorMessageID)
	responseIKEMessage.Payloads.Reset()

	var identifier uint8
	for {
		identifier = uint8(mathRand.Uint32())
		if identifier != ikeSecurityAssociation.LastEAPIdentifier {
			ikeSecurityAssociation.LastEAPIdentifier = identifier
			break
		}
	}

	responseIKEPayload.BuildEAPSuccess(identifier)

	if err := EncryptProcedure(ikeSecurityAssociation, responseIKEPayload, responseIKEMessage); err != nil {
		logger.IKELog.Errorf("encrypting IKE message failed: %+v", err)
		return
	}

	// Send IKE message to UE
	SendIKEMessageToUE(ikeSecurityAssociation.IKEConnection.Conn,
		ikeSecurityAssociation.IKEConnection.N3IWFAddr,
		ikeSecurityAssociation.IKEConnection.UEAddr, responseIKEMessage)

	ikeSecurityAssociation.State++
}

func HandleSendEAPNASMsg(ikeEvt context.IkeEvt) {
	logger.IKELog.Debugln("handle SendEAPNASMsg event")

	sendEAPNASMsgEvt := ikeEvt.(*context.SendEAPNASMsgEvt)
	localSPI := sendEAPNASMsgEvt.LocalSPI
	nasPDU := sendEAPNASMsgEvt.NasPDU

	n3iwfSelf := context.N3IWFSelf()
	ikeSecurityAssociation, _ := n3iwfSelf.IKESALoad(localSPI)

	responseIKEMessage := new(ike_message.IKEMessage)
	var responseIKEPayload ike_message.IKEPayloadContainer

	// Build IKE message
	responseIKEMessage.BuildIKEHeader(ikeSecurityAssociation.RemoteSPI,
		ikeSecurityAssociation.LocalSPI, ike_message.IKE_AUTH, ike_message.ResponseBitCheck,
		ikeSecurityAssociation.InitiatorMessageID)
	responseIKEMessage.Payloads.Reset()

	var identifier uint8
	for {
		identifier = uint8(mathRand.Uint32())
		if identifier != ikeSecurityAssociation.LastEAPIdentifier {
			ikeSecurityAssociation.LastEAPIdentifier = identifier
			break
		}
	}

	responseIKEPayload.BuildEAP5GNAS(identifier, nasPDU)

	if err := EncryptProcedure(ikeSecurityAssociation, responseIKEPayload, responseIKEMessage); err != nil {
		logger.IKELog.Errorf("encrypting IKE message failed: %+v", err)
		return
	}

	// Send IKE message to UE
	SendIKEMessageToUE(ikeSecurityAssociation.IKEConnection.Conn,
		ikeSecurityAssociation.IKEConnection.N3IWFAddr,
		ikeSecurityAssociation.IKEConnection.UEAddr, responseIKEMessage)
}

func HandleCreatePDUSession(ikeEvt context.IkeEvt) {
	logger.IKELog.Debugln("handle CreatePDUSession event")

	createPDUSessionEvt := ikeEvt.(*context.CreatePDUSessionEvt)
	localSPI := createPDUSessionEvt.LocalSPI
	temporaryPDUSessionSetupData := createPDUSessionEvt.TempPDUSessionSetupData

	n3iwfSelf := context.N3IWFSelf()
	ikeSecurityAssociation, _ := n3iwfSelf.IKESALoad(localSPI)

	ikeSecurityAssociation.IkeUE.PduSessionListLen = createPDUSessionEvt.PduSessionListLen

	CreatePDUSessionChildSA(ikeSecurityAssociation.IkeUE, temporaryPDUSessionSetupData)
}

func HandleIKEDeleteRequest(ikeEvt context.IkeEvt) {
	logger.IKELog.Debugln("handle IKEDeleteRequest event")

	ikeDeleteRequest := ikeEvt.(*context.IKEDeleteRequestEvt)
	localSPI := ikeDeleteRequest.LocalSPI

	SendIKEDeleteRequest(localSPI)
}

func HandleSendChildSADeleteRequest(ikeEvt context.IkeEvt) {
	logger.IKELog.Debugln("handle SendChildSADeleteRequest event")

	sendChildSADeleteRequestEvt := ikeEvt.(*context.SendChildSADeleteRequestEvt)
	localSPI := sendChildSADeleteRequestEvt.LocalSPI
	releaseIdList := sendChildSADeleteRequestEvt.ReleaseIdList

	ikeUe, ok := context.N3IWFSelf().IkeUePoolLoad(localSPI)
	if !ok {
		logger.IKELog.Errorf("cannot get IkeUE from SPI: %+v", localSPI)
		return
	}
	SendChildSADeleteRequest(ikeUe, releaseIdList)
}

func HandleIKEContextUpdate(ikeEvt context.IkeEvt) {
	logger.IKELog.Debugln("handle IKEContextUpdate event")

	ikeContextUpdateEvt := ikeEvt.(*context.IKEContextUpdateEvt)
	localSPI := ikeContextUpdateEvt.LocalSPI
	kn3iwf := ikeContextUpdateEvt.Kn3iwf

	ikeUe, ok := context.N3IWFSelf().IkeUePoolLoad(localSPI)
	if !ok {
		logger.IKELog.Errorf("cannot get IkeUE from SPI: %+v", localSPI)
		return
	}

	if kn3iwf != nil {
		ikeUe.Kn3iwf = kn3iwf
	}
}

func HandleGetNGAPContextResponse(ikeEvt context.IkeEvt) {
	logger.IKELog.Debugln("handle GetNGAPContextResponse event")

	getNGAPContextRepEvt := ikeEvt.(*context.GetNGAPContextRepEvt)
	localSPI := getNGAPContextRepEvt.LocalSPI
	ngapCxtReqNumlist := getNGAPContextRepEvt.NgapCxtReqNumlist
	ngapCxt := getNGAPContextRepEvt.NgapCxt

	n3iwfSelf := context.N3IWFSelf()
	ikeSecurityAssociation, _ := n3iwfSelf.IKESALoad(localSPI)

	var tempPDUSessionSetupData *context.PDUSessionSetupTemporaryData

	for i, num := range ngapCxtReqNumlist {
		switch num {
		case context.CxtTempPDUSessionSetupData:
			tempPDUSessionSetupData = ngapCxt[i].(*context.PDUSessionSetupTemporaryData)
		default:
			logger.IKELog.Errorf("receive undefined NGAP context request number: %d", num)
		}
	}

	switch ikeSecurityAssociation.State {
	case EndSignalling:
		CreatePDUSessionChildSA(ikeSecurityAssociation.IkeUE, tempPDUSessionSetupData)
		ikeSecurityAssociation.State++
		go StartDPD(ikeSecurityAssociation.IkeUE)
	case HandleCreateChildSA:
		continueCreateChildSA(ikeSecurityAssociation, tempPDUSessionSetupData)
	}
}

func CreatePDUSessionChildSA(ikeUe *context.N3IWFIkeUe,
	temporaryPDUSessionSetupData *context.PDUSessionSetupTemporaryData,
) {
	n3iwfSelf := context.N3IWFSelf()
	ikeSecurityAssociation := ikeUe.N3IWFIKESecurityAssociation

	ranNgapId, ok := n3iwfSelf.NgapIdLoad(ikeUe.N3IWFIKESecurityAssociation.LocalSPI)
	if !ok {
		logger.IKELog.Errorf("cannot get RanNgapId from SPI: %+v", ikeUe.N3IWFIKESecurityAssociation.LocalSPI)
		return
	}

	for {
		if len(temporaryPDUSessionSetupData.UnactivatedPDUSession) > temporaryPDUSessionSetupData.Index {
			pduSession := temporaryPDUSessionSetupData.UnactivatedPDUSession[temporaryPDUSessionSetupData.Index]
			pduSessionID := pduSession.Id

			// Send CREATE_CHILD_SA to UE
			ikeMessage := new(ike_message.IKEMessage)
			var ikePayload ike_message.IKEPayloadContainer
			errStr := context.ErrNil

			// Build IKE message
			ikeMessage.BuildIKEHeader(ikeSecurityAssociation.RemoteSPI,
				ikeSecurityAssociation.LocalSPI, ike_message.CREATE_CHILD_SA,
				0, ikeSecurityAssociation.ResponderMessageID)
			ikeMessage.Payloads.Reset()

			// Build SA
			requestSA := ikePayload.BuildSecurityAssociation()

			// Allocate SPI
			var spi uint32
			spiByte := make([]byte, 4)
			for {
				randomUint64 := GenerateRandomNumber().Uint64()
				if _, ok := n3iwfSelf.ChildSA.Load(uint32(randomUint64)); !ok {
					spi = uint32(randomUint64)
					break
				}
			}
			binary.BigEndian.PutUint32(spiByte, spi)

			// First Proposal - Proposal No.1
			proposal := requestSA.Proposals.BuildProposal(1, ike_message.TypeESP, spiByte)

			// Encryption transform
			var attributeType uint16 = ike_message.AttributeTypeKeyLength
			var attributeValue uint16 = 256
			proposal.EncryptionAlgorithm.BuildTransform(ike_message.TypeEncryptionAlgorithm,
				ike_message.ENCR_AES_CBC, &attributeType, &attributeValue, nil)
			// Integrity transform
			if pduSession.SecurityIntegrity {
				proposal.IntegrityAlgorithm.BuildTransform(ike_message.TypeIntegrityAlgorithm,
					ike_message.AUTH_HMAC_SHA1_96, nil, nil, nil)
			}

			// ESN transform
			proposal.ExtendedSequenceNumbers.BuildTransform(ike_message.TypeExtendedSequenceNumbers,
				ike_message.ESN_NO, nil, nil, nil)

			ikeUe.CreateHalfChildSA(ikeMessage.MessageID, spi, pduSessionID)

			// Build Nonce
			nonceData := GenerateRandomNumber().Bytes()
			ikePayload.BuildNonce(nonceData)

			// Store nonce into context
			ikeSecurityAssociation.ConcatenatedNonce = nonceData

			// TSi
			n3iwfIPAddr := net.ParseIP(n3iwfSelf.IpSecGatewayAddress)
			tsi := ikePayload.BuildTrafficSelectorInitiator()
			tsi.TrafficSelectors.BuildIndividualTrafficSelector(
				ike_message.TS_IPV4_ADDR_RANGE, ike_message.IPProtocolAll,
				0, 65535, n3iwfIPAddr.To4(), n3iwfIPAddr.To4())

			// TSr
			ueIPAddr := ikeUe.IPSecInnerIP
			tsr := ikePayload.BuildTrafficSelectorResponder()
			tsr.TrafficSelectors.BuildIndividualTrafficSelector(ike_message.TS_IPV4_ADDR_RANGE, ike_message.IPProtocolAll,
				0, 65535, ueIPAddr.To4(), ueIPAddr.To4())

			// Notify-Qos
			ikePayload.BuildNotify5G_QOS_INFO(uint8(pduSessionID), pduSession.QFIList, true, false, 0)

			// Notify-UP_IP_ADDRESS
			ikePayload.BuildNotifyUP_IP4_ADDRESS(n3iwfSelf.IpSecGatewayAddress)

			temporaryPDUSessionSetupData.Index++

			if err := EncryptProcedure(ikeUe.N3IWFIKESecurityAssociation, ikePayload, ikeMessage); err != nil {
				logger.IKELog.Errorf("encrypting IKE message failed: %+v", err)
				errStr = context.ErrTransportResourceUnavailable
				temporaryPDUSessionSetupData.FailedErrStr = append(temporaryPDUSessionSetupData.FailedErrStr,
					errStr)
				continue
			}

			temporaryPDUSessionSetupData.FailedErrStr = append(temporaryPDUSessionSetupData.FailedErrStr,
				errStr)

			SendIKEMessageToUE(ikeSecurityAssociation.IKEConnection.Conn, ikeSecurityAssociation.IKEConnection.N3IWFAddr,
				ikeSecurityAssociation.IKEConnection.UEAddr, ikeMessage)
			break
		} else {
			n3iwfSelf.NgapServer.RcvEventCh <- context.NewSendPDUSessionResourceSetupResEvt(
				ranNgapId,
			)
			break
		}
	}
}

func is_supported(transformType uint8, transformID uint16, attributePresent bool, attributeValue uint16) bool {
	switch transformType {
	case ike_message.TypeEncryptionAlgorithm:
		switch transformID {
		case ike_message.ENCR_DES_IV64:
			return false
		case ike_message.ENCR_DES:
			return false
		case ike_message.ENCR_3DES:
			return false
		case ike_message.ENCR_RC5:
			return false
		case ike_message.ENCR_IDEA:
			return false
		case ike_message.ENCR_CAST:
			return false
		case ike_message.ENCR_BLOWFISH:
			return false
		case ike_message.ENCR_3IDEA:
			return false
		case ike_message.ENCR_DES_IV32:
			return false
		case ike_message.ENCR_NULL:
			return false
		case ike_message.ENCR_AES_CBC:
			if !attributePresent {
				return false
			}
			switch attributeValue {
			case 128:
				return true
			case 192:
				return true
			case 256:
				return true
			default:
				return false
			}
		case ike_message.ENCR_AES_CTR:
			return false
		default:
			return false
		}
	case ike_message.TypePseudorandomFunction:
		switch transformID {
		case ike_message.PRF_HMAC_MD5:
			return true
		case ike_message.PRF_HMAC_SHA1:
			return true
		case ike_message.PRF_HMAC_TIGER:
			return false
		default:
			return false
		}
	case ike_message.TypeIntegrityAlgorithm:
		switch transformID {
		case ike_message.AUTH_NONE:
			return false
		case ike_message.AUTH_HMAC_MD5_96:
			return true
		case ike_message.AUTH_HMAC_SHA1_96:
			return true
		case ike_message.AUTH_DES_MAC:
			return false
		case ike_message.AUTH_KPDK_MD5:
			return false
		case ike_message.AUTH_AES_XCBC_96:
			return false
		default:
			return false
		}
	case ike_message.TypeDiffieHellmanGroup:
		switch transformID {
		case ike_message.DH_NONE:
			return false
		case ike_message.DH_768_BIT_MODP:
			return false
		case ike_message.DH_1024_BIT_MODP:
			return true
		case ike_message.DH_1536_BIT_MODP:
			return false
		case ike_message.DH_2048_BIT_MODP:
			return true
		case ike_message.DH_3072_BIT_MODP:
			return false
		case ike_message.DH_4096_BIT_MODP:
			return false
		case ike_message.DH_6144_BIT_MODP:
			return false
		case ike_message.DH_8192_BIT_MODP:
			return false
		default:
			return false
		}
	default:
		return false
	}
}

func is_Kernel_Supported(
	transformType uint8, transformID uint16, attributePresent bool, attributeValue uint16,
) bool {
	switch transformType {
	case ike_message.TypeEncryptionAlgorithm:
		switch transformID {
		case ike_message.ENCR_DES_IV64:
			return false
		case ike_message.ENCR_DES:
			return true
		case ike_message.ENCR_3DES:
			return true
		case ike_message.ENCR_RC5:
			return false
		case ike_message.ENCR_IDEA:
			return false
		case ike_message.ENCR_CAST:
			if !attributePresent {
				return false
			}
			switch attributeValue {
			case 128:
				return true
			case 256:
				return false
			default:
				return false
			}
		case ike_message.ENCR_BLOWFISH:
			return true
		case ike_message.ENCR_3IDEA:
			return false
		case ike_message.ENCR_DES_IV32:
			return false
		case ike_message.ENCR_NULL:
			return true
		case ike_message.ENCR_AES_CBC:
			if !attributePresent {
				return false
			}
			switch attributeValue {
			case 128:
				return true
			case 192:
				return true
			case 256:
				return true
			default:
				return false
			}
		case ike_message.ENCR_AES_CTR:
			if !attributePresent {
				return false
			}
			switch attributeValue {
			case 128:
				return true
			case 192:
				return true
			case 256:
				return true
			default:
				return false
			}
		default:
			return false
		}
	case ike_message.TypeIntegrityAlgorithm:
		switch transformID {
		case ike_message.AUTH_NONE:
			return false
		case ike_message.AUTH_HMAC_MD5_96:
			return true
		case ike_message.AUTH_HMAC_SHA1_96:
			return true
		case ike_message.AUTH_DES_MAC:
			return false
		case ike_message.AUTH_KPDK_MD5:
			return false
		case ike_message.AUTH_AES_XCBC_96:
			return true
		default:
			return false
		}
	case ike_message.TypeDiffieHellmanGroup:
		switch transformID {
		case ike_message.DH_NONE:
			return false
		case ike_message.DH_768_BIT_MODP:
			return false
		case ike_message.DH_1024_BIT_MODP:
			return false
		case ike_message.DH_1536_BIT_MODP:
			return false
		case ike_message.DH_2048_BIT_MODP:
			return false
		case ike_message.DH_3072_BIT_MODP:
			return false
		case ike_message.DH_4096_BIT_MODP:
			return false
		case ike_message.DH_6144_BIT_MODP:
			return false
		case ike_message.DH_8192_BIT_MODP:
			return false
		default:
			return false
		}
	case ike_message.TypeExtendedSequenceNumbers:
		switch transformID {
		case ike_message.ESN_NO:
			return true
		case ike_message.ESN_NEED:
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func parseIPAddressInformationToChildSecurityAssociation(
	childSecurityAssociation *context.ChildSecurityAssociation,
	uePublicIPAddr net.IP,
	trafficSelectorLocal *ike_message.IndividualTrafficSelector,
	trafficSelectorRemote *ike_message.IndividualTrafficSelector,
) error {
	if childSecurityAssociation == nil {
		return errors.New("childSecurityAssociation is nil")
	}

	childSecurityAssociation.PeerPublicIPAddr = uePublicIPAddr
	childSecurityAssociation.LocalPublicIPAddr = net.ParseIP(context.N3IWFSelf().IkeBindAddress)

	logger.IKELog.Debugf("local TS: %+v", trafficSelectorLocal.StartAddress)
	logger.IKELog.Debugf("remote TS: %+v", trafficSelectorRemote.StartAddress)

	childSecurityAssociation.TrafficSelectorLocal = net.IPNet{
		IP:   trafficSelectorLocal.StartAddress,
		Mask: []byte{255, 255, 255, 255},
	}

	childSecurityAssociation.TrafficSelectorRemote = net.IPNet{
		IP:   trafficSelectorRemote.StartAddress,
		Mask: []byte{255, 255, 255, 255},
	}

	return nil
}

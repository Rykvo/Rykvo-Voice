package device

import (
	"errors"
	"time"
)

var (
	ErrSMSInvalidRecipient = errors.New("invalid SMS recipient")
	ErrSMSEmpty            = errors.New("SMS text is empty")
	ErrSMSTooLong          = errors.New("SMS exceeds one-message encoding limit")
)

type SMSEncoding string

func intPointer(value int) *int { return &value }

const (
	SMSEncodingGSM7Text SMSEncoding = "gsm7_text"
	SMSEncodingGSM7PDU  SMSEncoding = "gsm7_pdu"
	SMSEncodingUCS2PDU  SMSEncoding = "ucs2_pdu"
	SMSEncodingUTF8PDU  SMSEncoding = "utf8_pdu"
	SMSEncodingGB18030  SMSEncoding = "gb18030_pdu"
	SMSEncodingLatin1   SMSEncoding = "latin1_pdu"
	SMSEncoding8BitPDU  SMSEncoding = "8bit_pdu"
	SMSEncodingUnknown  SMSEncoding = "unknown"
)

type SMSStorageArea struct {
	Used  int `json:"used"`
	Total int `json:"total"`
}

type SMSStorageUsage struct {
	SM SMSStorageArea `json:"sm"`
	ME SMSStorageArea `json:"me"`
}

func (usage SMSStorageUsage) Known() bool {
	return usage.SM.Total > 0 || usage.ME.Total > 0
}

type SMSListing struct {
	Messages []SMSMessage
	Storage  SMSStorageUsage
}

type SMSStorageStatus string

const (
	SMSStatusReceivedUnread SMSStorageStatus = "received_unread"
	SMSStatusReceivedRead   SMSStorageStatus = "received_read"
	SMSStatusStoredUnsent   SMSStorageStatus = "stored_unsent"
	SMSStatusStoredSent     SMSStorageStatus = "stored_sent"
	SMSStatusUnknown        SMSStorageStatus = "unknown"
)

type SMSDirection string

const (
	SMSDirectionReceived     SMSDirection = "received"
	SMSDirectionSubmitted    SMSDirection = "submitted"
	SMSDirectionStatusReport SMSDirection = "status_report"
	SMSDirectionUnknown      SMSDirection = "unknown"
)

type SMSConcatInfo struct {
	Reference int `json:"reference"`
	Total     int `json:"total"`
	Sequence  int `json:"sequence"`
}

// SMSSubmitTPDU is one modem-independent SMS-SUBMIT transfer unit. TPDU does
// not include the SMSC-length octet used by AT+CMGS PDU mode, so it can be
// embedded directly in an RP-DATA message for SMS over IMS.
type SMSSubmitTPDU struct {
	To              string
	Encoding        SMSEncoding
	TPDU            []byte
	Part            int
	Total           int
	ConcatReference *int
}

type SMSMessage struct {
	Index                  int              `json:"index"`
	Storage                string           `json:"storage,omitempty"`
	StorageStatus          SMSStorageStatus `json:"storageStatus"`
	Direction              SMSDirection     `json:"direction"`
	From                   string           `json:"from,omitempty"`
	To                     string           `json:"to,omitempty"`
	ServiceCenter          string           `json:"serviceCenter,omitempty"`
	Text                   string           `json:"text"`
	Encoding               SMSEncoding      `json:"encoding"`
	ServiceCenterTimestamp *time.Time       `json:"serviceCenterTimestamp,omitempty"`
	DischargeTimestamp     *time.Time       `json:"dischargeTimestamp,omitempty"`
	MessageReference       *int             `json:"messageReference,omitempty"`
	StatusCode             *int             `json:"statusCode,omitempty"`
	DeliveryStatus         string           `json:"deliveryStatus,omitempty"`
	Concat                 *SMSConcatInfo   `json:"concat,omitempty"`
	ProtocolID             int              `json:"protocolId"`
	DataCodingScheme       int              `json:"dataCodingScheme"`
	ModemLength            int              `json:"modemLength"`
	RawPDU                 string           `json:"rawPdu"`
	RawUserData            string           `json:"rawUserData,omitempty"`
	SIMDataDownload        bool             `json:"simDataDownload,omitempty"`
	DecodeError            string           `json:"decodeError,omitempty"`
}

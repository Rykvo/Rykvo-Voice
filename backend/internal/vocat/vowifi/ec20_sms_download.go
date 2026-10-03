package vowifi

import (
	"bytes"
	"context"
	"errors"
)

type SMSPPResult struct {
	Status uint16
	Data   []byte
}

// SMSPPDownload shares the existing UICC lock and never changes radio mode.
func (adapter *EC20Adapter) SMSPPDownload(ctx context.Context, identity SIMIdentity, tpdu, rpdu []byte) (SMSPPResult, error) {
	envelope, err := smsPPEnvelope(tpdu, rpdu)
	if err != nil {
		return SMSPPResult{}, err
	}
	defer clear(envelope)
	binding, err := adapter.bindingFor(identity)
	if err != nil {
		return SMSPPResult{}, err
	}
	adapter.apduMu.Lock()
	defer adapter.apduMu.Unlock()
	if locker, ok := adapter.executor.(EC20UICCLocker); ok {
		locker.LockUICC()
		defer locker.UnlockUICC()
	}
	if err = adapter.verifyLiveICCID(ctx, binding); err != nil {
		return SMSPPResult{}, err
	}
	if binding.application != "USIM" || binding.aid == "" {
		return SMSPPResult{}, ErrEC20ApplicationAbsent
	}
	if err = adapter.selectBasicApplication(ctx, binding.deviceID, binding.aid); err != nil {
		return SMSPPResult{}, err
	}
	apdu := append([]byte{0x80, 0xc2, 0, 0, byte(len(envelope))}, envelope...)
	apdu = append(apdu, 0) // Le: collect the UICC's delivery response.
	defer clear(apdu)
	raw, err := adapter.transmitBasicAPDU(ctx, binding.deviceID, apdu, true)
	if err != nil {
		return SMSPPResult{}, err
	}
	defer clear(raw)
	data, status, err := splitAPDUStatus(raw)
	if err != nil {
		return SMSPPResult{}, err
	}
	if err = adapter.verifyLiveICCID(ctx, binding); err != nil {
		return SMSPPResult{}, err
	}
	return SMSPPResult{Status: status, Data: bytes.Clone(data)}, nil
}

func smsPPEnvelope(tpdu, rpdu []byte) ([]byte, error) {
	invalid := errors.New("vocat: invalid SMS-PP download")
	if len(tpdu) < 1 || len(tpdu) > 232 || len(rpdu) < 5 || rpdu[0] != 1 {
		return nil, invalid
	}
	index := 2
	length := int(rpdu[index])
	index++
	if length > 12 || length > len(rpdu)-index {
		return nil, invalid
	}
	address := rpdu[index : index+length]
	index += length
	if index >= len(rpdu) {
		return nil, invalid
	}
	length = int(rpdu[index])
	index++
	if length > len(rpdu)-index {
		return nil, invalid
	}
	index += length
	if index >= len(rpdu) || int(rpdu[index]) != len(tpdu) || !bytes.Equal(rpdu[index+1:], tpdu) {
		return nil, invalid
	}
	body := []byte{0x82, 2, 0x83, 0x81} // Network -> UICC.
	if len(address) > 0 {
		body = append(body, 0x06, byte(len(address)))
		body = append(body, address...)
	}
	body = append(body, 0x8b)
	body = append(body, smsPPLength(len(tpdu))...)
	body = append(body, tpdu...)
	envelope := append([]byte{0xd1}, smsPPLength(len(body))...)
	envelope = append(envelope, body...)
	clear(body)
	if len(envelope) > 255 {
		clear(envelope)
		return nil, invalid
	}
	return envelope, nil
}

func smsPPLength(n int) []byte {
	if n < 128 {
		return []byte{byte(n)}
	}
	return []byte{0x81, byte(n)}
}

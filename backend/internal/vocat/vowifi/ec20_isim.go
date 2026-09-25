package vowifi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

var _ ProvisionedIMSIdentityReader = (*EC20Adapter)(nil)

// ReadProvisionedIMSIdentity performs only SELECT/READ operations. Discovery,
// reads and channel cleanup are one serialized UICC transaction. AKA continues
// to use its original application until the IMS layer explicitly selects ISIM.
func (adapter *EC20Adapter) ReadProvisionedIMSIdentity(ctx context.Context, identity SIMIdentity) (result *ProvisionedIMSIdentity, err error) {
	binding, err := adapter.bindingFor(identity)
	if err != nil {
		return nil, err
	}
	adapter.apduMu.Lock()
	defer adapter.apduMu.Unlock()
	if locker, ok := adapter.executor.(EC20UICCLocker); ok {
		locker.LockUICC()
		defer locker.UnlockUICC()
	}
	if err = adapter.verifyLiveICCID(ctx, binding); err != nil {
		return nil, err
	}
	basicTouched := false
	defer func() {
		if basicTouched && binding.aid != "" {
			cleanup, cancel := context.WithTimeout(context.Background(), channelCleanupTimeout)
			defer cancel()
			if restoreErr := adapter.selectBasicApplication(cleanup, binding.deviceID, binding.aid); restoreErr != nil {
				result = nil
				err = errors.New("vocat: ISIM application cleanup failed")
			}
		}
	}()

	var aid string
	response, probeErr := adapter.execute(ctx, binding.deviceID, "AT+CUAD")
	if probeErr == nil {
		data, parseErr := parseCUADData(response)
		if parseErr == nil {
			for _, candidate := range collectApplicationAIDs(data) {
				if strings.HasPrefix(candidate, isimAIDPrefix) {
					aid = candidate
					break
				}
			}
			if aid == "" && len(collectApplicationAIDs(data)) > 0 {
				return nil, ErrISIMUnavailable
			}
		}
	}
	if aid == "" {
		basicTouched = true
		aid, err = adapter.discoverBasicApplicationAID(ctx, binding.deviceID, isimAIDPrefix)
		if err != nil {
			if errors.Is(err, ErrEC20ApplicationAbsent) {
				return nil, ErrISIMUnavailable
			}
			return nil, errors.New("vocat: ISIM application discovery failed")
		}
	}
	channel, openErr := adapter.openLogicalChannel(ctx, binding.deviceID, aid)
	var exchange func([]byte) ([]byte, error)
	if openErr == nil {
		defer func() {
			if closeErr := adapter.closeLogicalChannelWithCleanup(binding.deviceID, channel); closeErr != nil {
				result = nil
				err = errors.Join(errors.New("vocat: ISIM channel cleanup failed"), err)
			}
		}()
		exchange = func(apdu []byte) ([]byte, error) {
			return adapter.transmitLogicalAPDU(ctx, binding.deviceID, channel, apdu, true)
		}
	} else {
		basicTouched = true
		if err = adapter.selectBasicApplication(ctx, binding.deviceID, aid); err != nil {
			return nil, errors.New("vocat: ISIM application selection failed")
		}
		exchange = func(apdu []byte) ([]byte, error) {
			return adapter.transmitBasicAPDU(ctx, binding.deviceID, apdu, true)
		}
	}
	result, err = readISIMIdentityFiles(exchange)
	if err != nil {
		return nil, err
	}
	if err = adapter.verifyLiveICCID(ctx, binding); err != nil {
		return nil, err
	}
	binding.isimAID, binding.isimBasicChannel = aid, openErr != nil
	adapter.mu.Lock()
	adapter.bindings[binding.iccid] = binding
	adapter.mu.Unlock()
	return result, nil
}

type isimFile struct {
	size, recordLength, records int
}

func parseISIMFile(data []byte, record bool) (isimFile, error) {
	invalid := errors.New("vocat: invalid ISIM file descriptor")
	tag, _, body, n, err := decodeBERTLV(data)
	if err != nil || len(tag) != 1 || tag[0] != 0x62 || n != len(data) {
		return isimFile{}, invalid
	}
	var result isimFile
	var descriptor []byte
	sizeSeen, descriptorSeen := false, false
	for len(body) > 0 {
		tag, _, value, n, err := decodeBERTLV(body)
		if err != nil {
			return isimFile{}, invalid
		}
		if len(tag) == 1 && tag[0] == 0x80 {
			if sizeSeen || len(value) < 1 || len(value) > 2 {
				return isimFile{}, invalid
			}
			sizeSeen = true
			for _, b := range value {
				result.size = result.size<<8 | int(b)
			}
		}
		if len(tag) == 1 && tag[0] == 0x82 {
			if descriptorSeen {
				return isimFile{}, invalid
			}
			descriptorSeen, descriptor = true, value
		}
		body = body[n:]
	}
	if result.size < 1 || result.size > 4096 || len(descriptor) < 2 {
		return isimFile{}, invalid
	}
	if record {
		if descriptor[0]&7 != 2 || len(descriptor) != 5 {
			return isimFile{}, invalid
		}
		result.recordLength = int(descriptor[2])<<8 | int(descriptor[3])
		result.records = int(descriptor[4])
		if result.recordLength < 1 || result.recordLength > 256 || result.records < 1 || result.records > 32 || result.recordLength*result.records != result.size {
			return isimFile{}, invalid
		}
	} else if descriptor[0]&7 != 1 {
		return isimFile{}, invalid
	}
	return result, nil
}

func readISIMIdentityFiles(transmit func([]byte) ([]byte, error)) (*ProvisionedIMSIdentity, error) {
	exchange := func(apdu []byte) ([]byte, error) {
		raw, err := transmit(apdu)
		if err != nil {
			// Never surface AT responses or provisioned subscription identifiers.
			return nil, errors.New("vocat: ISIM read exchange failed")
		}
		body, sw, err := splitAPDUStatus(raw)
		if err != nil || sw != 0x9000 {
			return nil, fmt.Errorf("vocat: ISIM read status %04X", sw)
		}
		return body, nil
	}
	result := &ProvisionedIMSIdentity{}
	for _, fileID := range []uint16{0x6f02, 0x6f03, 0x6f04} {
		fcp, err := exchange([]byte{0x00, 0xa4, 0x00, 0x04, 0x02, byte(fileID >> 8), byte(fileID), 0x00})
		if err != nil {
			return nil, err
		}
		file, err := parseISIMFile(fcp, fileID == 0x6f04)
		if err != nil {
			return nil, err
		}
		if fileID == 0x6f04 {
			for record := 1; record <= file.records; record++ {
				data, err := exchange([]byte{0x00, 0xb2, byte(record), 0x04, byte(file.recordLength)})
				if err != nil {
					return nil, err
				}
				if len(data) != file.recordLength {
					return nil, errors.New("vocat: short ISIM record")
				}
				if len(bytes.Trim(data, "\xff")) == 0 {
					continue // unused linear-fixed record, not an identity
				}
				value, err := decodeISIMString(data)
				if err != nil {
					return nil, err
				}
				result.PublicIdentities = append(result.PublicIdentities, value)
			}
			continue
		}
		var data []byte
		for offset := 0; offset < file.size; {
			length := min(255, file.size-offset)
			part, err := exchange([]byte{0x00, 0xb0, byte(offset >> 8), byte(offset), byte(length)})
			if err != nil {
				return nil, err
			}
			if len(part) != length {
				return nil, errors.New("vocat: short ISIM transparent file")
			}
			data = append(data, part...)
			offset += length
		}
		value, err := decodeISIMString(data)
		if err != nil {
			return nil, err
		}
		if fileID == 0x6f02 {
			result.PrivateIdentity = value
		} else {
			result.Domain = value
		}
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return result, nil
}

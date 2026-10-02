package vowifi

// CallFailure carries only a classified cause across the private audio bridge.
type CallFailure string

func (f CallFailure) Valid() bool {
	switch f {
	case "", "busy", "rejected", "no_answer", "remote_cancelled", "unsupported_audio":
		return true
	}
	return false
}

package hardware

import (
	"context"
	"sync"
	"time"
)

type esimCacheEntry struct {
	identity string
	expires  time.Time
	info     *ESIMInfo
}
type esimCache struct {
	sync.Mutex
	entries map[string]*esimCacheEntry
}

func cloneESIM(info *ESIMInfo) *ESIMInfo {
	if info == nil {
		return nil
	}
	copy := *info
	copy.Profiles = append([]ESIMProfile(nil), info.Profiles...)
	if info.FreeMemory != nil {
		memory := *info.FreeMemory
		copy.FreeMemory = &memory
	}
	return &copy
}

// Invalidate at card boundaries and before verifying an explicit eSIM write.
func (s *System) InvalidateESIM(c Candidate) {
	s.esims.Lock()
	delete(s.esims.entries, c.Key)
	s.esims.Unlock()
}

func (s *System) readESIMInventory(ctx context.Context, c Candidate, r Reading, read func(context.Context, ESIMRequest, func(string)) ESIMResult) *ESIMInfo {
	identity := Digest(c.Generation + ":" + c.Identity(r) + ":" + r.ICCID)
	cacheable := c.Generation != "" && r.ICCID != "" && CommunicationHealthy(r)
	now := time.Now()
	s.esims.Lock()
	if entry := s.esims.entries[c.Key]; cacheable && entry != nil && entry.identity == identity && now.Before(entry.expires) {
		result := cloneESIM(entry.info)
		s.esims.Unlock()
		return result
	}
	if s.esims.entries == nil {
		s.esims.entries = map[string]*esimCacheEntry{}
	}
	for key, value := range s.esims.entries {
		if !now.Before(value.expires) {
			delete(s.esims.entries, key)
		}
	}
	entry := &esimCacheEntry{identity: identity}
	s.esims.entries[c.Key] = entry
	s.esims.Unlock()
	call, cancel := context.WithTimeout(ctx, 30*time.Second)
	result := read(call, ESIMRequest{Candidate: c, Action: "read", ExpectedIMEI: r.IMEI}, nil)
	cancel()
	info := result.Info
	if info == nil {
		info = &ESIMInfo{Issue: result.Issue}
	}
	ttl := 15 * time.Second
	if info.Issue == "" && info.EID != "" {
		ttl = 2 * time.Minute
	} else if info.Issue == "NO_EUICC" {
		ttl = 10 * time.Minute
	}
	s.esims.Lock()
	defer s.esims.Unlock()
	if s.esims.entries[c.Key] == entry {
		if cacheable && ctx.Err() == nil {
			entry.info = cloneESIM(info)
			entry.expires = time.Now().Add(ttl)
		} else {
			delete(s.esims.entries, c.Key)
		}
	}
	return info
}

package ims

import (
	"fmt"
	"strconv"
	"strings"
)

func opusAttributes(pt byte) []string {
	return []string{fmt.Sprintf("a=rtpmap:%d opus/48000/2", pt),
		fmt.Sprintf("a=fmtp:%d maxplaybackrate=8000;sprop-maxcapturerate=8000;maxaveragebitrate=16000;stereo=0;sprop-stereo=0;useinbandfec=0;usedtx=0", pt)}
}

func opusRemoteOptions(body []byte, payload int) (int, bool) {
	bitrate := 12000
	inAudio := false
	for _, raw := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line := strings.ToLower(strings.TrimSpace(raw))
		if strings.HasPrefix(line, "m=") {
			if inAudio {
				break
			}
			inAudio = strings.HasPrefix(line, "m=audio ")
		}
		if !inAudio {
			continue
		}
		if strings.HasPrefix(line, "a=maxptime:") {
			n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "a=maxptime:")), 64)
			if err != nil || !(n >= 20) {
				return 0, false
			}
		}
		if !strings.HasPrefix(line, "a=fmtp:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "a=fmtp:"))
		if len(fields) < 2 || fields[0] != strconv.Itoa(payload) {
			continue
		}
		for _, pair := range strings.Split(strings.Join(fields[1:], ""), ";") {
			key, val, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			if key == "maxaveragebitrate" {
				n, err := strconv.Atoi(val)
				if err != nil || n < 6000 || n > 510000 {
					return 0, false
				}
				bitrate = min(bitrate, n)
			}
		}
	}
	return bitrate, true
}

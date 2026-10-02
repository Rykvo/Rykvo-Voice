package ims

const (
	pcmHistory = 320
	pcmFade    = 480 // At most 60 ms of synthetic audio, then silence.
	pcmBlend   = 40
)

// Per-call G.711 concealment. Keep only real samples in the pitch history.
type pcmConcealer struct {
	history [pcmHistory]int16
	cycle   [120]int16
	pos, n  int
	lag     int
	lost    int
	blend   int
	anchor  int16
	last    int16
}

func (p *pcmConcealer) past(back int) int16 {
	return p.history[(p.pos-back+pcmHistory)%pcmHistory]
}

func (p *pcmConcealer) start() {
	p.anchor, p.lag = p.last, 0
	best := 0.7
	for lag := 20; lag <= min(len(p.cycle), p.n-80); lag++ {
		var dot, a, b int64
		for i := 1; i <= 80; i++ {
			x, y := int64(p.past(i)), int64(p.past(i+lag))
			dot, a, b = dot+x*y, a+x*x, b+y*y
		}
		if dot <= 0 || a == 0 || b == 0 {
			continue
		}
		score := float64(dot) * float64(dot) / (float64(a) * float64(b))
		if score > best {
			best, p.lag = score, lag
		}
	}
	for i := 0; i < p.lag; i++ {
		p.cycle[i] = p.past(p.lag - i)
	}
}

func (p *pcmConcealer) predict() int16 {
	if p.lost >= pcmFade {
		return 0
	}
	if p.lag == 0 {
		return int16(int32(p.anchor) * int32(max(0, pcmBlend-p.lost-1)) / pcmBlend)
	}
	v := int32(p.cycle[p.lost%p.lag])
	if p.lost < pcmBlend {
		v = (int32(p.anchor)*int32(pcmBlend-p.lost-1) + v*int32(p.lost+1)) / pcmBlend
	}
	return int16(v * int32(pcmFade-p.lost-1) / pcmFade)
}

func (p *pcmConcealer) sample(v int16, present bool) int16 {
	if !present {
		if p.lost == 0 {
			p.start()
		}
		p.last = p.predict()
		p.lost, p.blend = min(p.lost+1, pcmFade), 0
		return p.last
	}
	p.history[p.pos] = v
	p.pos, p.n = (p.pos+1)%pcmHistory, min(p.n+1, pcmHistory)
	out := v
	if p.lost > 0 {
		p.blend++
		out = int16((int32(p.predict())*int32(pcmBlend-p.blend) + int32(v)*int32(p.blend)) / pcmBlend)
		p.lost = min(p.lost+1, pcmFade)
		if p.blend == pcmBlend {
			p.lost, p.blend = 0, 0
		}
	}
	p.last = out
	return out
}

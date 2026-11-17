package media

// AudioRate is the sample rate of a screen share's sound as handed to
// ScreenOptions.Audio (the voice engine's rate).
const AudioRate = 48000

// audioResampler hands captured sound on at AudioRate. ScreenCaptureKit is
// asked for 48 kHz and gives it; anything else is resampled linearly (good
// enough as a fallback, and never the usual path).
type audioResampler struct {
	out     func(pcm []float32, captureUS int64)
	started bool
	// pos: the read position in x = [last, in...] (x[0] is the previous
	// buffer's last sample, so interpolation runs across buffers).
	pos  float64
	last float32
	buf  []float32
}

func (a *audioResampler) push(in []float32, rate int, captureUS int64) {
	if rate == AudioRate || rate <= 0 {
		a.out(in, captureUS)
		return
	}
	if len(in) == 0 {
		return
	}
	if !a.started {
		a.started, a.pos = true, 1 // start on in[0]
	}
	x := func(i int) float32 {
		if i == 0 {
			return a.last
		}
		return in[i-1]
	}
	step := float64(rate) / AudioRate
	a.buf = a.buf[:0]
	for ; a.pos <= float64(len(in)); a.pos += step {
		i := int(a.pos)
		f := float32(a.pos - float64(i))
		v := x(i)
		if f > 0 && i < len(in) {
			v += (x(i+1) - v) * f
		}
		a.buf = append(a.buf, v)
	}
	a.pos -= float64(len(in))
	a.last = in[len(in)-1]
	if len(a.buf) > 0 {
		a.out(a.buf, captureUS)
	}
}

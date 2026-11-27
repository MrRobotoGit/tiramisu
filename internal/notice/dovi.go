package notice

import _ "embed"

var (
	//go:embed assets/rpu_p5.bin
	rpuP5 []byte
	//go:embed assets/rpu_p81.bin
	rpuP81 []byte
)

// RPUFor is the static RPU for a DV profile: 5, else 8.1 (also used for 7's base layer).
func RPUFor(profile int) []byte {
	if profile == 5 {
		return rpuP5
	}
	return rpuP81
}

// AppendRPU adds the RPU NAL (type 62) at the end of one access unit.
func AppendRPU(frame []byte, nalLen int, rpu []byte) []byte {
	out := append([]byte(nil), frame...)
	for i := nalLen - 1; i >= 0; i-- {
		out = append(out, byte(len(rpu)>>(8*i)))
	}
	return append(out, rpu...)
}

package srt

// splitAnnexB splits an H.264 Annex-B byte stream into NAL units (without the
// leading start codes or trailing zero padding).
func splitAnnexB(data []byte) [][]byte {
	var out [][]byte
	start := -1
	i := 0
	for i < len(data) {
		if i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			if start >= 0 {
				out = appendNALU(out, data[start:i])
			}
			i += 4
			start = i
			continue
		}
		if i+3 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if start >= 0 {
				out = appendNALU(out, data[start:i])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(data) {
		out = appendNALU(out, data[start:])
	}
	return out
}

func appendNALU(out [][]byte, b []byte) [][]byte {
	end := len(b)
	for end > 0 && b[end-1] == 0 {
		end--
	}
	if end == 0 {
		return out
	}
	return append(out, append([]byte(nil), b[:end]...))
}

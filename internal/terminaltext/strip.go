package terminaltext

import "strings"

const (
	stateText byte = iota
	stateEscape
	stateCSI
	stateString
	stateStringEscape
)

// Stripper removes ANSI/ECMA-48 control sequences while retaining parser
// state across writes. PTY reads may split one escape sequence across chunks.
type Stripper struct {
	state byte
}

func (s *Stripper) WriteString(input string) string {
	var output strings.Builder
	output.Grow(len(input))
	for index := 0; index < len(input); index++ {
		value := input[index]
		switch s.state {
		case stateText:
			switch value {
			case 0x1b:
				s.state = stateEscape
			case 0x9b:
				s.state = stateCSI
			case 0x90, 0x9d, 0x9e, 0x9f:
				s.state = stateString
			default:
				output.WriteByte(value)
			}
		case stateEscape:
			switch value {
			case '[':
				s.state = stateCSI
			case ']', 'P', 'X', '^', '_':
				s.state = stateString
			default:
				// Two-byte escape sequence.
				s.state = stateText
			}
		case stateCSI:
			if value >= 0x40 && value <= 0x7e {
				s.state = stateText
			}
		case stateString:
			switch value {
			case 0x07:
				s.state = stateText
			case 0x1b:
				s.state = stateStringEscape
			}
		case stateStringEscape:
			if value == '\\' {
				s.state = stateText
			} else if value != 0x1b {
				s.state = stateString
			}
		}
	}
	return output.String()
}

func Strip(input string) string {
	var stripper Stripper
	return stripper.WriteString(input)
}

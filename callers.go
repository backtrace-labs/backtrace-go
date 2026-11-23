package bt

func isUpperASCII(s string) bool {
	return s != "" && 'A' <= s[0] && s[0] <= 'Z'
}

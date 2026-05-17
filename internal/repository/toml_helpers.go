package repository

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func stripInlineComment(line string) string {
	inString := false
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && inString {
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if r == '#' && !inString {
			return line[:i]
		}
	}
	return line
}

func parseString(raw string, out *string) error {
	value, err := strconv.Unquote(raw)
	if err != nil {
		return err
	}
	*out = value
	return nil
}

func parseInt(raw string, out *int) error {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return err
	}
	*out = value
	return nil
}

func writeString(b *strings.Builder, key, value string) {
	fmt.Fprintf(b, "%s = %s\n", key, strconv.Quote(value))
}

func writeInt(b *strings.Builder, key string, value int) {
	fmt.Fprintf(b, "%s = %d\n", key, value)
}

func writeStringArray(b *strings.Builder, key string, values []string) {
	data, _ := json.Marshal(values)
	fmt.Fprintf(b, "%s = %s\n", key, data)
}

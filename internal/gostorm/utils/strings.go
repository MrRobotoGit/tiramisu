package utils

import (
	"strconv"
	"strings"
	"unicode"
)

const (
	_ = 1.0 << (10 * iota) // ignore first value by assigning to blank identifier
	KB
	MB
	GB
	TB
	PB
	EB
)

func CommonPrefix(first, second string) string {
	var result strings.Builder

	minLength := len(first)
	if len(second) < minLength {
		minLength = len(second)
	}

	for i := 0; i < minLength; i++ {
		if first[i] != second[i] {
			break
		}
		result.WriteByte(first[i])
	}

	return result.String()
}

func NumberPrefix(str string) (int, error) {
	var result strings.Builder

	for i := 0; i < len(str); i++ {
		if !unicode.IsDigit(rune(str[i])) {
			break
		}
		result.WriteByte(str[i])
	}

	return strconv.Atoi(result.String())
}

func CompareStrings(first, second string) bool {
	commonPrefix := CommonPrefix(first, second)
	resultStr1 := strings.TrimPrefix(first, commonPrefix)
	resultStr2 := strings.TrimPrefix(second, commonPrefix)
	num1, err1 := NumberPrefix(resultStr1)
	num2, err2 := NumberPrefix(resultStr2)

	if err1 == nil && err2 == nil {
		return num1 < num2
	}
	if err1 == nil {
		return true
	} else if err2 == nil {
		return false
	}
	return resultStr1 < resultStr2
}

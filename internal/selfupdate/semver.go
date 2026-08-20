package selfupdate

import (
	"fmt"
	"strconv"
	"strings"
)

type semver struct {
	major, minor, patch int
	pre                 []string
}

func parseSemver(value string) (semver, error) {
	if !strings.HasPrefix(value, "v") {
		return semver{}, fmt.Errorf("version %q must start with v", value)
	}
	value = strings.TrimPrefix(value, "v")
	if build := strings.IndexByte(value, '+'); build >= 0 {
		value = value[:build]
	}
	var pre []string
	if dash := strings.IndexByte(value, '-'); dash >= 0 {
		pre = strings.Split(value[dash+1:], ".")
		value = value[:dash]
		if len(pre) == 0 || pre[0] == "" {
			return semver{}, fmt.Errorf("version has an empty prerelease")
		}
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("version must contain major, minor, and patch")
	}
	numbers := make([]int, 3)
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semver{}, fmt.Errorf("version component %q is invalid", part)
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return semver{}, fmt.Errorf("version component %q is invalid", part)
		}
		numbers[i] = n
	}
	for _, identifier := range pre {
		if identifier == "" {
			return semver{}, fmt.Errorf("version has an empty prerelease identifier")
		}
		for _, r := range identifier {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-') {
				return semver{}, fmt.Errorf("prerelease identifier %q is invalid", identifier)
			}
		}
		if len(identifier) > 1 && identifier[0] == '0' && isNumeric(identifier) {
			return semver{}, fmt.Errorf("numeric prerelease identifier %q has a leading zero", identifier)
		}
	}
	return semver{major: numbers[0], minor: numbers[1], patch: numbers[2], pre: pre}, nil
}

func (v semver) compare(other semver) int {
	for _, pair := range [][2]int{{v.major, other.major}, {v.minor, other.minor}, {v.patch, other.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(v.pre) == 0 && len(other.pre) == 0 {
		return 0
	}
	if len(v.pre) == 0 {
		return 1
	}
	if len(other.pre) == 0 {
		return -1
	}
	for i := 0; i < len(v.pre) && i < len(other.pre); i++ {
		left, right := v.pre[i], other.pre[i]
		if left == right {
			continue
		}
		leftNumber, rightNumber := isNumeric(left), isNumeric(right)
		switch {
		case leftNumber && rightNumber:
			leftInt, _ := strconv.Atoi(left)
			rightInt, _ := strconv.Atoi(right)
			if leftInt < rightInt {
				return -1
			}
			return 1
		case leftNumber:
			return -1
		case rightNumber:
			return 1
		case left < right:
			return -1
		default:
			return 1
		}
	}
	if len(v.pre) < len(other.pre) {
		return -1
	}
	if len(v.pre) > len(other.pre) {
		return 1
	}
	return 0
}

func isNumeric(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

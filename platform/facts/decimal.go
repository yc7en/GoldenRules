package facts

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// parseInt 解析整数字面量。
func parseInt(s string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

// decimalPattern 与引擎侧 decimal 的落地形态一致（十进制字符串，可选符号与小数）。
var decimalPattern = func(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if _, ok := new(big.Rat).SetString(s); !ok {
		return false
	}
	// big.Rat 也接受 "1/3" 这类写法，契约只允许十进制。
	return !strings.Contains(s, "/")
}

// parseDecimal 校验十进制字符串（不引入浮点，A-5）。
func parseDecimal(s string) (*big.Rat, error) {
	if !decimalPattern(s) {
		return nil, fmt.Errorf("非法十进制: %q", s)
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return nil, fmt.Errorf("非法十进制: %q", s)
	}
	return r, nil
}

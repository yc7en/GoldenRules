package facts

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// parseInt parses an integer literal.
func parseInt(s string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

// decimalPattern matches how decimal is materialised on the engine side (decimal string, optional sign and fraction).
var decimalPattern = func(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if _, ok := new(big.Rat).SetString(s); !ok {
		return false
	}
	// big.Rat also accepts forms like "1/3"; the contract only allows decimal.
	return !strings.Contains(s, "/")
}

// parseDecimal validates a decimal string (no floats involved, A-5).
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

package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// Money is an amount in minor units (cents for USD). We never use floats for
// amounts.
type Money struct {
	Minor    int64
	Currency string
}

// currencyExponent covers the currencies used in the demo data. Anything not
// listed is assumed to have two decimals.
var currencyExponent = map[string]int{
	"JPY": 0,
	"CLP": 0,
	"KRW": 0,
	"BHD": 3,
	"KWD": 3,
}

func exponent(currency string) int {
	if e, ok := currencyExponent[currency]; ok {
		return e
	}
	return 2
}

// ParseMoney parses a decimal string like "1,299.90" for the given ISO 4217
// currency code.
func ParseMoney(value, currency string) (Money, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if len(currency) != 3 {
		return Money{}, fmt.Errorf("invalid currency %q", currency)
	}
	v := strings.ReplaceAll(strings.TrimSpace(value), ",", "")
	if v == "" {
		return Money{}, fmt.Errorf("empty amount")
	}
	if strings.HasPrefix(v, "-") {
		return Money{}, fmt.Errorf("negative amount %q", value)
	}

	exp := exponent(currency)
	whole, frac, _ := strings.Cut(v, ".")
	if len(frac) > exp {
		return Money{}, fmt.Errorf("amount %q has more than %d decimals for %s", value, exp, currency)
	}
	frac += strings.Repeat("0", exp-len(frac))

	minor, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("invalid amount %q", value)
	}
	return Money{Minor: minor, Currency: currency}, nil
}

// Decimal returns the amount as a plain decimal string, e.g. "129.90".
func (m Money) Decimal() string {
	exp := exponent(m.Currency)
	if exp == 0 {
		return strconv.FormatInt(m.Minor, 10)
	}
	s := fmt.Sprintf("%0*d", exp+1, m.Minor)
	return s[:len(s)-exp] + "." + s[len(s)-exp:]
}

func (m Money) String() string {
	return m.Decimal() + " " + m.Currency
}

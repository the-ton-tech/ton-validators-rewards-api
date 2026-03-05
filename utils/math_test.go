package utils

import (
	"math/big"
	"testing"
)

func TestMulDiv(t *testing.T) {
	tests := []struct {
		name string
		a    *big.Int
		b    *big.Int
		c    *big.Int
		want *big.Int
	}{
		{
			name: "basic multiplication and division",
			a:    big.NewInt(10),
			b:    big.NewInt(5),
			c:    big.NewInt(2),
			want: big.NewInt(25),
		},
		{
			name: "division by one",
			a:    big.NewInt(100),
			b:    big.NewInt(50),
			c:    big.NewInt(1),
			want: big.NewInt(5000),
		},
		{
			name: "division with truncation",
			a:    big.NewInt(10),
			b:    big.NewInt(5),
			c:    big.NewInt(3),
			want: big.NewInt(16), // 50/3 = 16.66... truncates to 16
		},
		{
			name: "zero numerator",
			a:    big.NewInt(0),
			b:    big.NewInt(100),
			c:    big.NewInt(5),
			want: big.NewInt(0),
		},
		{
			name: "large numbers",
			a:    big.NewInt(1000000),
			b:    big.NewInt(1000000),
			c:    big.NewInt(1000),
			want: big.NewInt(1000000000),
		},
		{
			name: "identity when c equals product",
			a:    big.NewInt(7),
			b:    big.NewInt(11),
			c:    big.NewInt(77),
			want: big.NewInt(1),
		},
		{
			name: "single factor",
			a:    big.NewInt(42),
			b:    big.NewInt(1),
			c:    big.NewInt(1),
			want: big.NewInt(42),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MulDiv(tt.a, tt.b, tt.c)
			if got.Cmp(tt.want) != 0 {
				t.Errorf("MulDiv() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMulDiv_DivisionByZero(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MulDiv with c=0 should panic")
		}
	}()
	MulDiv(big.NewInt(1), big.NewInt(1), big.NewInt(0))
}

func TestMulDiv_DoesNotMutateInputs(t *testing.T) {
	a := big.NewInt(10)
	b := big.NewInt(5)
	c := big.NewInt(2)
	aOrig := new(big.Int).Set(a)
	bOrig := new(big.Int).Set(b)
	cOrig := new(big.Int).Set(c)

	MulDiv(a, b, c)

	if a.Cmp(aOrig) != 0 || b.Cmp(bOrig) != 0 || c.Cmp(cOrig) != 0 {
		t.Error("MulDiv mutated input arguments")
	}
}

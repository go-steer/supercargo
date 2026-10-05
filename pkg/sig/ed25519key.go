// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sig

import (
	"errors"
	"math/big"
)

// Edwards25519 parameters: p = 2^255 - 19, d = -121665/121666 mod p.
var (
	edP = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	edD = func() *big.Int {
		inv := new(big.Int).ModInverse(big.NewInt(121666), edP)
		d := new(big.Int).Mul(big.NewInt(-121665), inv)
		return d.Mod(d, edP)
	}()
)

// checkEd25519PublicKey rejects public keys no honest key generator
// produces and that weaken signatures: encodings that are not canonical
// (y >= p, or x = 0 with the sign bit set), points not on the curve, and
// points of small order. Go's crypto/ed25519 accepts all of these, and its
// cofactorless verification lets anyone sign for a small-order key: the
// identity point, for one, verifies R = identity, S = 0 for every message.
// It runs once per key, so plain big.Int arithmetic is fine.
func checkEd25519PublicKey(pub []byte) error {
	sign := pub[31] >> 7
	le := make([]byte, 32)
	copy(le, pub)
	le[31] &= 0x7f
	y := new(big.Int).SetBytes(reverse(le))
	if y.Cmp(edP) >= 0 {
		return errors.New("sig: Ed25519 public key is not canonically encoded")
	}
	// x^2 = (y^2 - 1) / (d y^2 + 1)
	y2 := mulMod(y, y)
	u := new(big.Int).Sub(y2, big.NewInt(1))
	v := new(big.Int).Add(mulMod(edD, y2), big.NewInt(1))
	x2 := mulMod(u.Mod(u, edP), new(big.Int).ModInverse(v.Mod(v, edP), edP))
	x := new(big.Int).ModSqrt(x2, edP)
	if x == nil {
		return errors.New("sig: Ed25519 public key is not a point on the curve")
	}
	if x.Sign() == 0 && sign == 1 {
		return errors.New("sig: Ed25519 public key is not canonically encoded")
	}
	if x.Bit(0) != uint(sign) {
		x.Sub(edP, x)
	}
	// Small order means 8P is the identity (0, 1).
	px, py := x, y
	for range 3 {
		px, py = edAdd(px, py, px, py)
	}
	if px.Sign() == 0 && py.Cmp(big.NewInt(1)) == 0 {
		return errors.New("sig: Ed25519 public key has small order")
	}
	return nil
}

// edAdd is affine point addition on the twisted Edwards curve
// -x^2 + y^2 = 1 + d x^2 y^2. The formula is complete (d is not a square
// mod p), so it also doubles and never divides by zero.
func edAdd(x1, y1, x2, y2 *big.Int) (*big.Int, *big.Int) {
	t := mulMod(edD, mulMod(mulMod(x1, x2), mulMod(y1, y2)))
	xn := new(big.Int).Add(mulMod(x1, y2), mulMod(y1, x2))
	yn := new(big.Int).Add(mulMod(y1, y2), mulMod(x1, x2))
	xd := new(big.Int).Add(big.NewInt(1), t)
	yd := new(big.Int).Sub(big.NewInt(1), t)
	x3 := mulMod(xn, new(big.Int).ModInverse(xd.Mod(xd, edP), edP))
	y3 := mulMod(yn, new(big.Int).ModInverse(yd.Mod(yd, edP), edP))
	return x3, y3
}

func mulMod(a, b *big.Int) *big.Int {
	r := new(big.Int).Mul(a, b)
	return r.Mod(r, edP)
}

func reverse(b []byte) []byte {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b
}

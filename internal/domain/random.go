package domain

import (
	"crypto/rand"
	"math"
	"math/big"
)

// Intn returns an integer in [0, n).
type Random interface{ Intn(n int64) (int64, error) }
type CryptoRandom struct{}

func (CryptoRandom) Intn(n int64) (int64, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0, err
	}
	return v.Int64(), nil
}

// Select samples distinct people with integer weights, then selects an ordered reserve.
func Select(candidates []Candidate, winners, reserves int, random Random) (Selection, error) {
	if winners < 1 || reserves < 0 {
		return Selection{}, UserFail(Invalid, "Invalid prize count.", "Некорректное количество призов.")
	}
	pool := append([]Candidate(nil), candidates...)
	seen := map[int64]bool{}
	var total int64
	for _, c := range pool {
		if c.UserID <= 0 || c.Weight < 1 || seen[c.UserID] || int64(c.Weight) > math.MaxInt64-total {
			return Selection{}, UserFail(Invalid, "Invalid candidate list.", "Некорректный список участников.")
		}
		seen[c.UserID] = true
		total += int64(c.Weight)
	}
	result := Selection{Winners: []int64{}, Reserve: []int64{}}
	for i := 0; i < winners+reserves && len(pool) > 0; i++ {
		pick, err := random.Intn(total)
		if err != nil {
			return Selection{}, err
		}
		if pick < 0 || pick >= total {
			return Selection{}, UserFail(Invalid, "Random source returned an out-of-range value.", "Источник случайных чисел вернул некорректное значение.")
		}
		var index int
		for j, c := range pool {
			if pick < int64(c.Weight) {
				index = j
				break
			}
			pick -= int64(c.Weight)
		}
		selected := pool[index]
		if i < winners {
			result.Winners = append(result.Winners, selected.UserID)
		} else {
			result.Reserve = append(result.Reserve, selected.UserID)
		}
		total -= int64(selected.Weight)
		pool = append(pool[:index], pool[index+1:]...)
	}
	return result, nil
}

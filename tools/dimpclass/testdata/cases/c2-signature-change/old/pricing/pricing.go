package pricing

import (
	"context"
	"fmt"
)

var catalog = map[string]int{"apple": 120, "pear": 90}

func lookupPrice(id string) (int, error) {
	p, ok := catalog[id]
	if !ok {
		return 0, fmt.Errorf("unknown product %q", id)
	}
	return p, nil
}

// Quote prices a cart in cents.
func Quote(ctx context.Context, ids []string) (int, error) {
	total := 0
	for _, id := range ids {
		p, err := lookupPrice(id)
		if err != nil {
			return 0, err
		}
		total += p
	}
	return total, nil
}

// Single prices one item.
func Single(ctx context.Context, id string) (int, error) { return lookupPrice(id) }

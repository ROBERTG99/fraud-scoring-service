package replay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
)

// GroupLag returns how many records in topics the consumer group has not committed yet. A partition the
// group never committed counts from its start offset.
func GroupLag(ctx context.Context, adm *kadm.Client, group string, topics []string) (int64, error) {
	ends, err := adm.ListEndOffsets(ctx, topics...)
	if err != nil {
		return 0, err
	}
	starts, err := adm.ListStartOffsets(ctx, topics...)
	if err != nil {
		return 0, err
	}
	committed, err := adm.FetchOffsets(ctx, group)
	if errors.Is(err, kerr.GroupIDNotFound) {
		committed, err = kadm.OffsetResponses{}, nil // the group never committed: everything is lag
	}
	if err != nil {
		return 0, err
	}
	var lag int64
	var perr error
	ends.Each(func(o kadm.ListedOffset) {
		if o.Err != nil {
			perr = o.Err
			return
		}
		from := int64(0)
		if s, ok := starts.Lookup(o.Topic, o.Partition); ok {
			from = s.Offset
		}
		if c, ok := committed.Lookup(o.Topic, o.Partition); ok && c.Err == nil && c.At >= 0 {
			from = max(from, c.At)
		}
		lag += max(0, o.Offset-from)
	})
	return lag, perr
}

// CaughtUp returns a WaitCaughtUp function that polls until group has no lag on topics.
func CaughtUp(adm *kadm.Client, group string, topics []string, poll time.Duration) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		t := time.NewTicker(poll)
		defer t.Stop()
		var last int64 = -1
		for {
			lag, err := GroupLag(ctx, adm, group, topics)
			if err == nil && lag == 0 {
				return nil
			}
			if err == nil {
				last = lag
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("group %s still %d records behind: %w", group, last, ctx.Err())
			case <-t.C:
			}
		}
	}
}

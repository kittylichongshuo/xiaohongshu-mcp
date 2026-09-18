package xiaohongshu

import (
	"context"
	"time"

	"github.com/go-rod/rod"
)

const searchReadinessTimeout = 20 * time.Second
const searchReadinessPoll = 200 * time.Millisecond

// Either state container may signal readiness; the existing extraction source
// selection remains unchanged. DOM uses exactly the extraction recognizer.
const searchReadinessJS = `(() => {
 const feeds = window.__INITIAL_STATE__?.search?.feeds;
 const valid = rows => Array.isArray(rows) && rows.some(f => f && f.modelType === 'note' && typeof f.id === 'string' && f.id.trim() && typeof f.xsecToken === 'string' && f.xsecToken.trim());
 if (valid(feeds?.value) || valid(feeds?._value)) return 'state';
 return (` + renderedSearchFeedsJS + `)().length ? 'DOM' : 'none';
})()`

func waitSearchResultReady(page *rod.Page) error {
	return waitForSearchReady(page.GetContext(), searchReadinessTimeout, func(ctx context.Context) (bool, error) {
		value, err := evaluateSearchValue(page, ctx, searchReadinessJS)
		if err != nil {
			return false, err
		}
		return value.Str() == "state" || value.Str() == "DOM", nil
	})
}

// Only this local readiness budget may expire normally. Parent cancellation
// and page deadlines always propagate, including a race with the local timer.
// Polling observes the same page; it never repeats navigation/search requests.
func waitForSearchReady(parent context.Context, budget time.Duration, probe func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	for {
		if err := parent.Err(); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		ready, err := probe(ctx)
		if parent.Err() != nil {
			return parent.Err()
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		timer := time.NewTimer(searchReadinessPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			if parent.Err() != nil {
				return parent.Err()
			}
			return nil
		case <-timer.C:
		}
	}
}

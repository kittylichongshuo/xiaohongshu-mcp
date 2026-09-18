package xiaohongshu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

// Shared by readiness and extraction. Reads only rendered note-card links,
// never author/content/image fields. Tokens remain in the runtime Feed only.
// Production structure: search-layout__main > feeds-container > note-item.
// Only cover/title hrefs supply access parameters; hidden placeholder anchors
// and links outside the search result list are not candidates.
const renderedSearchFeedsJS = `() => {
 const out = [], seen = new Set();
 const cards = document.querySelectorAll('div.search-layout__main div.feeds-container > section.note-item');
 for (let index = 0; index < cards.length; index++) {
  const card = cards[index], rect = card.getBoundingClientRect(), style = getComputedStyle(card);
  if (rect.width <= 0 || rect.height <= 0 || style.display === 'none' || style.visibility === 'hidden') continue;
  for (const link of card.querySelectorAll('a.cover[href], a.title[href]')) {
   const linkRect = link.getBoundingClientRect(), linkStyle = getComputedStyle(link);
   if (linkRect.width <= 0 || linkRect.height <= 0 || linkStyle.display === 'none' || linkStyle.visibility === 'hidden') continue;
   let url;
   try { url = new URL(link.getAttribute('href'), window.location.origin); } catch (_) { continue; }
   if (url.protocol !== 'https:' || !['www.xiaohongshu.com','xiaohongshu.com'].includes(url.hostname) || url.username || url.password || url.port) continue;
   const match = url.pathname.match(/^\/(?:explore|discovery\/item|search_result)\/([a-zA-Z0-9_-]+)\/?$/);
   const token = url.searchParams.get('xsec_token');
   if (!match || !token || !token.trim()) continue;
   const id = match[1];
   if (!seen.has(id)) {
    seen.add(id);
    out.push({id, xsecToken:token, modelType:'note', noteCard:{}, index});
   }
   break;
  }
 }
 return out;
}`

// Raw CDP evaluation has no implicit Rod evaluation retry. Errors carry no
// remote exception text, href, token or page content.
func evaluateSearchValue(page *rod.Page, ctx context.Context, expression string) (gson.JSON, error) {
	result, err := (proto.RuntimeEvaluate{Expression: expression, ReturnByValue: true, Silent: true}).Call(page.Context(ctx))
	if err != nil {
		return gson.New(nil), err
	}
	if result.ExceptionDetails != nil || result.Result == nil {
		return gson.New(nil), errors.New("search page evaluation failed")
	}
	return result.Result.Value, nil
}

func extractRenderedSearchFeeds(page *rod.Page) ([]Feed, error) {
	value, err := evaluateSearchValue(page, page.GetContext(), "("+renderedSearchFeedsJS+")()")
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("invalid rendered search results")
	}
	var feeds []Feed
	if json.Unmarshal(raw, &feeds) != nil {
		return nil, errors.New("invalid rendered search results")
	}
	if feeds == nil {
		feeds = []Feed{}
	}
	return feeds, nil
}

func hasUsableSearchNote(feeds []Feed) bool {
	for _, feed := range feeds {
		if feed.ModelType == modelTypeNote && strings.TrimSpace(feed.ID) != "" && strings.TrimSpace(feed.XsecToken) != "" {
			return true
		}
	}
	return false
}

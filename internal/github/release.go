package github

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"dbxdl/internal/naming"
)

// maxReleasePages bounds how many pages dbxdl scans while looking for
// application releases. One page holds 100 releases, and the repository mixes
// four tag families, so a handful of pages is plenty.
const maxReleasePages = 5

// AppReleases returns up to want releases whose tags match pattern, skipping
// drafts (and pre-releases unless includePre is set), newest version first.
//
// The dbx repository publishes several unrelated tag families from the same
// release feed ("v0.6.14", "packages-v0.4.88", "agents-v0.2.111"), so a plain
// "latest" lookup is not safe: the tag pattern decides what counts.
func (c *Client) AppReleases(ctx context.Context, owner, repo, pattern string, includePre bool, want int) ([]Release, error) {
	type candidate struct {
		release Release
		version naming.Version
	}
	var found []candidate

	for page := 1; page <= maxReleasePages; page++ {
		releases, err := c.ListReleases(ctx, owner, repo, page, 100)
		if err != nil {
			if len(found) > 0 {
				// Partial success: the versions already collected are still
				// useful, but the caller deserves to know the list is short.
				if c.log != nil {
					c.log.Warnf("stopping release scan early: %v", err)
				}
				break
			}
			return nil, err
		}
		for _, rel := range releases {
			if rel.Draft {
				continue
			}
			if rel.Prerelease && !includePre {
				continue
			}
			version, ok := naming.ExtractVersion(rel.TagName, pattern)
			if !ok {
				continue
			}
			if version.Pre != "" && !includePre {
				continue
			}
			found = append(found, candidate{release: rel, version: version})
		}
		if (want > 0 && len(found) >= want) || len(releases) < 100 {
			break
		}
	}

	sort.SliceStable(found, func(i, j int) bool {
		return naming.Compare(found[i].version, found[j].version) > 0
	})
	if want > 0 && len(found) > want {
		found = found[:want]
	}
	out := make([]Release, 0, len(found))
	for _, c := range found {
		out = append(out, c.release)
	}
	return out, nil
}

// LatestAppRelease returns the newest matching application release.
func (c *Client) LatestAppRelease(ctx context.Context, owner, repo, pattern string, includePre bool) (*Release, error) {
	releases, err := c.AppReleases(ctx, owner, repo, pattern, includePre, 1)
	if err != nil {
		return nil, err
	}
	if len(releases) == 0 {
		return nil, fmt.Errorf("no release of %s/%s matches tag_pattern %q", owner, repo, pattern)
	}
	return &releases[0], nil
}

// ResolveRelease finds a release for an explicit version. It first asks for
// the canonical "v<version>" tag and then falls back to scanning the release
// list, which tolerates repositories that tag without the "v" prefix.
func (c *Client) ResolveRelease(ctx context.Context, owner, repo, pattern string, version naming.Version) (*Release, error) {
	rel, err := c.ReleaseByTag(ctx, owner, repo, version.Tag())
	if err == nil {
		return rel, nil
	}
	if !IsNotFound(err) {
		return nil, err
	}

	candidates := []string{version.String()}
	if version.Pre == "" {
		candidates = append(candidates, "v"+version.Core(), version.Core())
	}
	for _, tag := range candidates {
		if tag == version.Tag() {
			continue
		}
		rel, retryErr := c.ReleaseByTag(ctx, owner, repo, tag)
		if retryErr == nil {
			return rel, nil
		}
		if !IsNotFound(retryErr) {
			return nil, retryErr
		}
	}

	// Last resort: walk the release feed and match on the extracted version.
	releases, listErr := c.AppReleases(ctx, owner, repo, pattern, true, 0)
	if listErr != nil {
		return nil, err
	}
	for i := range releases {
		got, ok := naming.ExtractVersion(releases[i].TagName, pattern)
		if ok && naming.Compare(got, version) == 0 {
			return &releases[i], nil
		}
	}
	return nil, fmt.Errorf("release for version %s not found in %s/%s", version.String(), owner, repo)
}

// AtomVersions returns the versions published in the public Atom feed, newest
// first, filtered by the same tag pattern as the API path.
//
// The feed carries no pre-release flag, so a version with a pre-release suffix
// is treated as a pre-release. It is used as a quota-free fallback when the
// REST API is rate limited.
func (c *Client) AtomVersions(ctx context.Context, owner, repo, pattern string, includePre bool, want int) ([]naming.Version, error) {
	tags, err := c.ReleasesAtom(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var versions []naming.Version
	for _, tag := range tags {
		version, ok := naming.ExtractVersion(tag, pattern)
		if !ok || seen[version.String()] {
			continue
		}
		if version.Pre != "" && !includePre {
			continue
		}
		seen[version.String()] = true
		versions = append(versions, version)
	}
	sort.SliceStable(versions, func(i, j int) bool { return naming.Compare(versions[i], versions[j]) > 0 })
	if want > 0 && len(versions) > want {
		versions = versions[:want]
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("no tag matching %q found in the Atom feed of %s/%s", pattern, owner, repo)
	}
	return versions, nil
}

// atomFeed mirrors the subset of the releases Atom feed dbxdl parses.
type atomFeed struct {
	Entries []struct {
		Title   string `xml:"title"`
		Updated string `xml:"updated"`
		Link    struct {
			Href string `xml:"href,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

// ReleasesAtom returns tag names from the public Atom feed. It requires no API
// quota and is used as a fallback when the REST API is rate limited, so
// "dbxdl list" keeps working anonymously.
func (c *Client) ReleasesAtom(ctx context.Context, owner, repo string) ([]string, error) {
	rawURL := fmt.Sprintf("%s/%s/%s/releases.atom",
		c.webBase, url.PathEscape(owner), url.PathEscape(repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/atom+xml")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: read body: %w", rawURL, err)
	}
	var feed atomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("fetch %s: parse atom feed: %w", rawURL, err)
	}
	tags := make([]string, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		// The tag is the final path element of .../releases/tag/<tag>.
		if idx := strings.LastIndex(e.Link.Href, "/"); idx >= 0 && idx < len(e.Link.Href)-1 {
			tags = append(tags, e.Link.Href[idx+1:])
		}
	}
	return tags, nil
}

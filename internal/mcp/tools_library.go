package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"tiramisu/internal/library"
)

const (
	defaultListLimit = 100
	maxListLimit     = 500
)

type ListInput struct {
	Type   string `json:"type" jsonschema:"movie, tv, music or audiobook"`
	Query  string `json:"query,omitempty" jsonschema:"video: case-insensitive substring of the file name"`
	IMDB   string `json:"imdb,omitempty" jsonschema:"movie: exact IMDb id"`
	Offset int    `json:"offset,omitempty" jsonschema:"video paging"`
	Limit  int    `json:"limit,omitempty" jsonschema:"default 100, max 500"`
	Prefix string `json:"prefix,omitempty" jsonschema:"audio: path prefix inside the section"`
	Cursor string `json:"cursor,omitempty" jsonschema:"audio paging: next_cursor of the previous page"`
}

type ListOutput struct {
	Items      []library.Item          `json:"items,omitempty"`
	Total      int                     `json:"total,omitempty"`
	Audio      []library.AudioListItem `json:"audio,omitempty"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

// GapsInput is empty; a named type keeps the schema an object in every SDK version.
type GapsInput struct{}

type GapsOutput struct {
	Gaps  []library.Gap `json:"gaps"`
	Total int           `json:"total"`
}

type RemoveInput struct {
	Type      string   `json:"type" jsonschema:"movie, tv, music or audiobook"`
	Paths     []string `json:"paths,omitempty" jsonschema:"fuse_path or path of each entry"`
	Prefix    string   `json:"prefix,omitempty" jsonschema:"audio only: every projection under this album prefix"`
	Blacklist *bool    `json:"blacklist" jsonschema:"required: true removes for good (the sync will not re-add it), false removes to replace"`
	Confirm   bool     `json:"confirm,omitempty" jsonschema:"false (default) only previews; true removes"`
}

type RemoveOutput struct {
	Preview    bool     `json:"preview"`
	Entries    []string `json:"entries"`
	Unresolved []string `json:"unresolved,omitempty"`
	Removed    []string `json:"removed,omitempty"`
	Failed     []string `json:"failed,omitempty"`
}

func isAudio(kind string) bool { return kind == "music" || kind == "audiobook" }

func (s *server) registerLibrary(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "list_library",
		Description: "List what the library holds. Video is filtered and paged here; audio pages with a cursor.",
	}, s.listLibrary)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "list_gaps",
		Description: "TV episodes the reaper removed because their release died and nothing live replaced them. The engine re-searches them on its own; never re-add a dead_hash.",
	}, s.listGaps)
	destructive := true
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "remove_entries",
		Description: "Remove library entries. Without confirm it only previews (what would go, what was not found). blacklist is required: true = for good, false = to replace.",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: &destructive},
	}, s.removeEntries)
}

func (s *server) listLibrary(_ context.Context, _ *sdk.CallToolRequest, in ListInput) (*sdk.CallToolResult, ListOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = defaultListLimit
	} else if limit > maxListLimit {
		limit = maxListLimit
	}
	if isAudio(in.Type) {
		resp, err := s.deps.Library.ListAudio(library.AudioListRequest{Type: in.Type, Prefix: in.Prefix, Limit: limit, Cursor: in.Cursor})
		if err != nil {
			return errResult(err), ListOutput{}, nil
		}
		return nil, ListOutput{Audio: resp.Items, NextCursor: resp.NextCursor}, nil
	}
	if in.Type != "movie" && in.Type != "tv" {
		return errResult(fmt.Errorf("type must be movie, tv, music or audiobook, got %q", in.Type)), ListOutput{}, nil
	}
	items, err := s.deps.Library.List(in.Type)
	if err != nil {
		return errResult(err), ListOutput{}, nil
	}
	q := strings.ToLower(in.Query)
	var hits []library.Item
	for _, it := range items {
		if q != "" && !strings.Contains(strings.ToLower(filepath.Base(it.FusePath)), q) {
			continue
		}
		if in.IMDB != "" && it.IMDB != in.IMDB {
			continue
		}
		hits = append(hits, it)
	}
	out := ListOutput{Total: len(hits), Items: []library.Item{}}
	if in.Offset >= 0 && in.Offset < len(hits) {
		out.Items = hits[in.Offset:min(in.Offset+limit, len(hits))]
	}
	return nil, out, nil
}

func (s *server) listGaps(_ context.Context, _ *sdk.CallToolRequest, _ GapsInput) (*sdk.CallToolResult, GapsOutput, error) {
	gaps, total, err := s.deps.Library.ListGaps()
	if err != nil {
		return errResult(err), GapsOutput{}, nil
	}
	if gaps == nil {
		gaps = []library.Gap{}
	}
	return nil, GapsOutput{Gaps: gaps, Total: total}, nil
}

func (s *server) removeEntries(ctx context.Context, _ *sdk.CallToolRequest, in RemoveInput) (*sdk.CallToolResult, RemoveOutput, error) {
	if in.Blacklist == nil {
		return errResult(fmt.Errorf("blacklist is required: ask the user whether this removal is for good or to replace")), RemoveOutput{}, nil
	}
	if in.Prefix != "" && !isAudio(in.Type) {
		return errResult(fmt.Errorf("prefix is for music and audiobook only")), RemoveOutput{}, nil
	}
	if in.Prefix == "" && len(in.Paths) == 0 {
		return errResult(fmt.Errorf("paths or prefix is required")), RemoveOutput{}, nil
	}
	entries, unresolved, err := s.resolveEntries(in)
	if err != nil {
		return errResult(err), RemoveOutput{}, nil
	}
	out := RemoveOutput{Preview: !in.Confirm, Entries: entries, Unresolved: unresolved}
	if !in.Confirm {
		return nil, out, nil
	}
	var reqs []library.RemoveRequest
	if in.Prefix != "" {
		reqs = append(reqs, library.RemoveRequest{Type: in.Type, Prefix: in.Prefix, Blacklist: *in.Blacklist})
	} else {
		for _, p := range entries {
			reqs = append(reqs, library.RemoveRequest{Type: in.Type, Path: p, Blacklist: *in.Blacklist})
		}
	}
	for _, r := range reqs {
		target := r.Path + r.Prefix
		if _, err := s.deps.Library.RemoveAny(ctx, r); err != nil {
			out.Failed = append(out.Failed, target+": "+err.Error())
			continue
		}
		out.Removed = append(out.Removed, target)
	}
	return nil, out, nil
}

// resolveEntries checks each requested path against what the library lists.
func (s *server) resolveEntries(in RemoveInput) (found, missing []string, err error) {
	found = []string{}
	known := map[string]bool{}
	if isAudio(in.Type) {
		cursor := ""
		for {
			page, err := s.deps.Library.ListAudio(library.AudioListRequest{Type: in.Type, Prefix: in.Prefix, Limit: maxListLimit, Cursor: cursor})
			if err != nil {
				return nil, nil, err
			}
			for _, it := range page.Items {
				known[it.Path] = true
				if in.Prefix != "" {
					found = append(found, it.Path)
				}
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		if in.Prefix != "" {
			return found, nil, nil
		}
	} else {
		items, err := s.deps.Library.List(in.Type)
		if err != nil {
			return nil, nil, err
		}
		for _, it := range items {
			known[it.Path], known[it.FusePath] = true, true
		}
	}
	for _, p := range in.Paths {
		if known[p] {
			found = append(found, p)
		} else {
			missing = append(missing, p)
		}
	}
	return found, missing, nil
}

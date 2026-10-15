package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"tiramisu/internal/library"
)

type InspectInput struct {
	Hash         string `json:"hash,omitempty"`
	Magnet       string `json:"magnet,omitempty"`
	Title        string `json:"title" jsonschema:"the torrent's display name; required"`
	TorrentFile  string `json:"torrent_file,omitempty" jsonschema:"the release's .torrent, base64"`
	MetadataWait int    `json:"metadata_wait,omitempty" jsonschema:"seconds, default 60, max 300"`
}

type AudioInput struct {
	Type        string                     `json:"type" jsonschema:"music or audiobook"`
	Hash        string                     `json:"hash,omitempty"`
	Magnet      string                     `json:"magnet,omitempty"`
	Title       string                     `json:"title"`
	TorrentFile string                     `json:"torrent_file,omitempty" jsonschema:"the release's .torrent, base64"`
	Files       []library.AudioFileRequest `json:"files" jsonschema:"one entry per track: source_path from inspect_torrent, path you choose inside the section"`
}

func (s *server) registerAudio(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "inspect_torrent",
		Description: "List a torrent's files (audio adds name each file). The torrent comes from the caller: there is no audio search here. Returns a job_id.",
	}, s.inspectTorrent)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "add_audio",
		Description: "File tracks of a music or audiobook torrent. You choose each virtual path; the _<hash8> suffix is appended if missing. All files land or none. Returns a job_id.",
	}, s.addAudio)
}

func decodeTorrent(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("torrent_file is not base64: %w", err)
	}
	return data, nil
}

func (s *server) inspectTorrent(_ context.Context, _ *sdk.CallToolRequest, in InspectInput) (*sdk.CallToolResult, JobStarted, error) {
	file, err := decodeTorrent(in.TorrentFile)
	if err != nil {
		return errResult(err), JobStarted{}, nil
	}
	req := library.InspectRequest{Hash: in.Hash, Magnet: in.Magnet, Title: in.Title, TorrentFile: file, MetadataWait: in.MetadataWait}
	job, err := s.jobs.Start("inspect", func(ctx context.Context) (any, error) { return s.deps.Library.Inspect(ctx, req) })
	if err != nil {
		return errResult(err), JobStarted{}, nil
	}
	return nil, JobStarted{JobID: job.ID}, nil
}

func (s *server) addAudio(_ context.Context, _ *sdk.CallToolRequest, in AudioInput) (*sdk.CallToolResult, JobStarted, error) {
	if !isAudio(in.Type) {
		return errResult(fmt.Errorf("type must be music or audiobook, got %q", in.Type)), JobStarted{}, nil
	}
	hash := in.Hash
	if in.Magnet != "" {
		hash = library.HashFromMagnet(in.Magnet)
	}
	token, ok := library.HashSuffixToken(hash)
	if !ok {
		return errResult(fmt.Errorf("a 40-character hash (or a magnet carrying one) is required")), JobStarted{}, nil
	}
	file, err := decodeTorrent(in.TorrentFile)
	if err != nil {
		return errResult(err), JobStarted{}, nil
	}
	files := make([]library.AudioFileRequest, len(in.Files))
	for i, f := range in.Files {
		if !library.HasHashSuffix(f.Path, hash) {
			ext := path.Ext(f.Path)
			f.Path = strings.TrimSuffix(f.Path, ext) + "_" + token + ext
		}
		files[i] = f
	}
	req := library.AddRequest{Type: in.Type, Hash: hash, Magnet: in.Magnet, Title: in.Title, TorrentFile: file, Files: files}
	job, err := s.jobs.Start("add_audio", func(ctx context.Context) (any, error) { return s.deps.Library.AddAudio(ctx, req) })
	if err != nil {
		return errResult(err), JobStarted{}, nil
	}
	return nil, JobStarted{JobID: job.ID}, nil
}

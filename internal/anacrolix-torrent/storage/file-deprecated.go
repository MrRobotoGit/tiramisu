package storage

func NewFileWithCompletion(baseDir string, completion PieceCompletion) ClientImplCloser {
	return NewFileWithCustomPathMakerAndCompletion(baseDir, nil, completion)
}

// Deprecated: Allows passing custom PieceCompletion
func NewFileWithCustomPathMakerAndCompletion(
	baseDir string,
	pathMaker TorrentDirFilePathMaker,
	completion PieceCompletion,
) ClientImplCloser {
	return NewFileOpts(NewFileClientOpts{
		ClientBaseDir:   baseDir,
		TorrentDirMaker: pathMaker,
		PieceCompletion: completion,
	})
}

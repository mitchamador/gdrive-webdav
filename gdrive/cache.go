package gdrive

import (
	"time"

	log "github.com/cihub/seelog"
)

const (
	cacheKeyAbout = "global:about"
	cacheKeyFile  = "file:"
	cacheKeyDir  = "file:"
)

func (fs *fileSystem) invalidatePath(p string) {
	log.Tracef("invalidatePath %v", p)
	fs.cache.Delete(cacheKeyFile + p)
}

type fileLookupResult struct {
	fp  *fileAndPath
	err error
}

func (fs *fileSystem) getFile(p string, onlyFolder bool) (*fileAndPath, error) {
	key := cacheKeyFile + p

	if lookup, found := fs.cache.Get(key); found {
		log.Tracef("getFile cache hit %v %v", p, onlyFolder)
		result := lookup.(*fileLookupResult)
		return result.fp, result.err
	}

	log.Tracef("getFile %v %v", p, onlyFolder)

	fp, err := fs.getFile0(p, onlyFolder)
	lookup := &fileLookupResult{fp: fp, err: err}
	if err == nil {
		fs.cache.Set(key, lookup, time.Minute)
	}
	return lookup.fp, lookup.err
}

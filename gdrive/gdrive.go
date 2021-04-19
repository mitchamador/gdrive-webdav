package gdrive

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"io"

	log "github.com/cihub/seelog"
	gocache "github.com/pmylund/go-cache"
	"golang.org/x/net/context"
	"golang.org/x/net/webdav"
	"google.golang.org/api/drive/v3"
)

type fileSystem struct {
	client       *drive.Service
	roundTripper http.RoundTripper
	cache        *gocache.Cache
}

const (
	mimeTypeFolder = "application/vnd.google-apps.folder"
)

type fileAndPath struct {
	file  *drive.File
	path  string
	files []*drive.File
}

// NewFS creates new gdrive file system.
func NewFS(ctx context.Context, clientID string, clientSecret string) webdav.FileSystem {
	httpClient := newHTTPClient(ctx, clientID, clientSecret)
	client, err := drive.New(httpClient)
	if err != nil {
		log.Errorf("An error occurred creating Drive client: %v\n", err)
		panic(-3)
	}

	fs := &fileSystem{
		client:       client,
		roundTripper: httpClient.Transport,
		cache:        gocache.New(5*time.Minute, 30*time.Second),
	}
	return fs
}

// NewLS creates new GDrive locking system
func NewLS() webdav.LockSystem {
	return webdav.NewMemLS()
}

func (fs *fileSystem) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	log.Debugf("Mkdir %v %v", name, perm)
	name = normalizePath(name)
	pID, err := fs.getFileID(name, false)
	if err != nil && err != os.ErrNotExist {
		log.Error(err)
		return err
	}
	if err == nil {
		log.Errorf("dir already exists: %v", pID)
		return os.ErrExist
	}

	parent := path.Dir(name)
	dir := path.Base(name)

	parentID, err := fs.getFileID(parent, true)
	if err != nil {
		return err
	}

	if parentID == "" {
		log.Errorf("parent not found")
		return os.ErrNotExist
	}

	f := &drive.File{
		MimeType: mimeTypeFolder,
		Name:     dir,
		Parents:  []string{parentID},
	}

	_, err = fs.client.Files.Create(f).Do()
	if err != nil {
		return err
	}

	fs.invalidatePath(name)
	fs.invalidatePath(parent)

	return nil
}

type openWritableFile struct {
	ctx        context.Context
	fileSystem *fileSystem
	buffer     bytes.Buffer
	size       int64
	name       string
	flag       int
	perm       os.FileMode
}

func (f *openWritableFile) Write(p []byte) (int, error) {
	n, err := f.buffer.Write(p)
	f.size += int64(n)
	return n, err
}

func (f *openWritableFile) Readdir(count int) ([]os.FileInfo, error) {
	panic("not supported")
}

func (f *openWritableFile) Stat() (os.FileInfo, error) {
	return &fileInfo{
		isDir:        false,
		size:         f.size,
	}, nil
}

func (f *openWritableFile) Close() error {
	log.Debugf("Close %v", f.name)
	fs := f.fileSystem
	fileID, err := fs.getFileID(f.name, false)
	if err != nil && err != os.ErrNotExist {
		log.Error(err)
		return err
	}

	if fileID != "" {
		err = os.ErrExist
		log.Error(err)
		return err
	}

	parent := path.Dir(f.name)
	base := path.Base(f.name)

	parentID, err := fs.getFileID(parent, true)
	if err != nil {
		log.Error(err)
		return err
	}

	if parentID == "" {
		err = os.ErrNotExist
		log.Error(err)
		return err
	}

	file := &drive.File{
		Name:    base,
		Parents: []string{parentID},
	}

	_, err = fs.client.Files.Create(file).Media(&f.buffer).Do()
	if err != nil {
		log.Error(err)
		return err
	}

	fs.invalidatePath(f.name)
	fs.invalidatePath(parent)

	log.Debug("Close succesfull ", f.name)
	return nil
}
func (f *openWritableFile) Read(p []byte) (n int, err error) {
	log.Critical("not implemented")
	panic("not implemented")
}
func (f *openWritableFile) Seek(offset int64, whence int) (int64, error) {
	log.Critical("not implemented")
	panic("not implemented")
}

type openReadonlyFile struct {
	fs            *fileSystem
	file          *drive.File
	content       []byte
	size          int64
	pos           int64
	contentReader io.Reader
	name          string
	body		  io.ReadCloser
}

func (f *openReadonlyFile) Write(p []byte) (int, error) {
	log.Critical("not implemented")
	panic("not implemented")
}

func (f *openReadonlyFile) Readdir(count int) ([]os.FileInfo, error) {

	files := []os.FileInfo{}
	aLookup := &fileLookupResult{}

	if lookup, found := f.fs.cache.Get(cacheKeyDir + f.file.Id); found {
		log.Trace("Reusing cached file: ", f.file.Id)
		aLookup = lookup.(*fileLookupResult)
	} else {
		query := fmt.Sprintf("'%s' in parents", f.file.Id)
		r, err := f.fs.client.Files.List().Q(query).Fields("files(id,name,mimeType,trashed,parents,size,parents,createdTime,modifiedTime)").Do()

		if err != nil {
			log.Error("Can't list children ", err)
			return nil, err
		}

		lookup := &fileLookupResult{fp: &fileAndPath{
			file:  f.file,
			path:  f.file.Id,
			files: r.Files,
		}, err: nil}

		f.fs.cache.Set(cacheKeyDir + lookup.fp.path, lookup, 5*time.Second)

		aLookup = lookup
	}

	for _, file := range aLookup.fp.files {
		if ignoreFile(file) {
			continue
		}
		files = append(files, newFileInfo(file))

		lookup := &fileLookupResult{fp: &fileAndPath{
			file: file,
			path: f.name + "/" + file.Name,
		}, err: nil}

		f.fs.cache.Set(cacheKeyFile + lookup.fp.path, lookup, time.Minute)
	}

	return files, nil
}

func (f *openReadonlyFile) Stat() (os.FileInfo, error) {
	return newFileInfo(f.file), nil
}

func (f *openReadonlyFile) Close() error {
	log.Debug("Close ", f.name)
	f.content = nil
	if f.body != nil {
		f.body.Close()
	}
	f.contentReader = nil
	return nil
}

func (f *openReadonlyFile) initContentReader() error {
	if f.contentReader != nil {
		return nil
	}

	// Get timeout reader wrapper and context
	timeoutReaderWrapper, ctx := getTimeoutReaderWrapperContext(time.Second * 15)

	res, err := f.fs.client.Files.Get(f.file.Id).Context(ctx).Download()

	if err != nil {
		if err == context.Canceled {
			log.Errorf("Failed to download file: timeout, no data was transferred for %v", time.Second*15)
			return err
		}
		log.Errorf("Failed to download file: %s", err)
		return err
	}

	f.body = res.Body
	f.contentReader = timeoutReaderWrapper(f.body)

	return nil
}

func (f *openReadonlyFile) Read(p []byte) (n int, err error) {
	log.Debug("Read ", len(p))
	//err = f.initContent()
	err = f.initContentReader()

	if err != nil {
		log.Error(err)
		return 0, err
	}

	n, err = f.contentReader.Read(p)
	if err != nil {
		log.Error(err)
		return 0, err
	}

	f.pos += int64(n)
	return n, err
}

func (f *openReadonlyFile) Seek(offset int64, whence int) (int64, error) {
	log.Debug("Seek ", offset, whence)

	if whence == 0 {
		//// io.SeekStart
		//if f.content != nil {
		//	f.pos = 0
		//	f.contentReader = bytes.NewBuffer(f.content)
		//	return 0, nil
		//}
		f.pos = 0
		return f.pos, nil
	}

	if whence == 2 {
		//// io.SeekEnd
		//err := f.initContent()
		//if err != nil {
		//	return 0, err
		//}
		//f.contentReader = &bytes.Buffer{}
		//f.pos = f.size
		f.size = f.file.Size
		f.pos = f.size
		return f.pos, nil
	}

	panic("not implemented")
}

func (fs *fileSystem) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	log.Debugf("OpenFile %v %v %v", name, flag, perm)
	name = normalizePath(name)

	if flag&os.O_RDWR != 0 {
		if flag != os.O_RDWR|os.O_CREATE|os.O_TRUNC {
			panic("not implemented")
		}

		return &openWritableFile{
			ctx:        ctx,
			fileSystem: fs,
			name:       name,
			flag:       flag,
			perm:       perm,
		}, nil
	}

	if flag == os.O_RDONLY {
		file, err := fs.getFile(name, false)
		if err != nil {
			return nil, err
		}
		return &openReadonlyFile{fs: fs, file: file.file, name: name}, nil
	}

	return nil, fmt.Errorf("unsupported open mode: %v", flag)
}

func (fs *fileSystem) RemoveAll(ctx context.Context, name string) error {
	log.Debugf("RemoveAll %v", name)
	name = normalizePath(name)
	id, err := fs.getFileID(name, false)
	if err != nil {
		return err
	}

	err = fs.client.Files.Delete(id).Do()
	if err != nil {
		log.Errorf("can't delete file %v", err)
		return err
	}

	fs.invalidatePath(name)
	fs.invalidatePath(path.Dir(name))
	return nil

}
func (fs *fileSystem) Rename(ctx context.Context, oldName, newName string) error {
	log.Critical("not implemented")
	panic("not implemented")
}

type fileInfo struct {
	name         string
	isDir        bool
	modTime      time.Time
	size         int64
}


func (fi *fileInfo) ContentType(ctx context.Context) (string, error) {
	return "application/octet-stream", nil
}

func newFileInfo(file *drive.File) *fileInfo {
	modTime, err := getModTime(file)
	if err != nil {
		log.Error(err)
		panic(err)
	}

	return &fileInfo{
		name:         file.Name,
		isDir:        file.MimeType == mimeTypeFolder,
		modTime:      modTime,
		size:         file.Size,
	}
}

func getModTime(file *drive.File) (time.Time, error) {
	modifiedTime := file.ModifiedTime
	if modifiedTime == "" {
		modifiedTime = file.CreatedTime
	}
	if modifiedTime == "" {
		return time.Time{}, nil
	}

	modTime, err := time.Parse(time.RFC3339, modifiedTime)
	if err != nil {
		return time.Time{}, err
	}

	return modTime, nil
}

func (fi *fileInfo) IsDir() bool {
	return fi.isDir
}

func (fi *fileInfo) Name() string {
	return fi.name
}

func (fi *fileInfo) Size() int64 {
	return fi.size
}
func (fi *fileInfo) Mode() os.FileMode {
	log.Critical("not implemented")
	panic("not implemented")
}
func (fi *fileInfo) ModTime() time.Time {
	return fi.modTime
}
func (fi *fileInfo) Sys() interface{} {
	return fi
}

func (fs *fileSystem) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	log.Debugf("Stat %v", name)
	f, err := fs.getFile(name, false)

	if err != nil {
		log.Error(err)
		return nil, err
	}

	if f == nil {
		log.Debug("Can't find file ", name)
		return nil, os.ErrNotExist
	}

	return newFileInfo(f.file), nil
}

func (fs *fileSystem) getFileID(p string, onlyFolder bool) (string, error) {
	f, err := fs.getFile(p, onlyFolder)

	if err != nil {
		return "", err
	}

	return f.file.Id, nil
}

func (fs *fileSystem) getFile0(p string, onlyFolder bool) (*fileAndPath, error) {
	log.Tracef("getFile0 %v %v", p, onlyFolder)
	p = normalizePath(p)

	if p == "" {
		f, err := fs.client.Files.Get("root").Do()
		if err != nil {
			log.Error(err)
			return nil, err
		}
		return &fileAndPath{file: f, path: "/"}, nil
	}

	parent := path.Dir(p)
	base := path.Base(p)

	parentID, err := fs.getFileID(parent, true)
	if err != nil {
		log.Errorf("can't locate parent %v error: %v", parent, err)
		return nil, err
	}

	q := fs.client.Files.List()
	query := fmt.Sprintf("'%s' in parents and name=\"%s\"", parentID, base)
	if onlyFolder {
		query += " and mimeType='" + mimeTypeFolder + "'"
	}
	q.Q(query)
	q.Fields("files(id,name,mimeType,trashed,parents,size,parents,createdTime,modifiedTime)")
	log.Tracef("Query: %v", query)

	r, err := q.Do()

	if err != nil {
		log.Error(err)
		return nil, err
	}

	for _, file := range r.Files {
		if ignoreFile(file) {
			continue
		}

		return &fileAndPath{file: file, path: p}, nil
	}

	return nil, os.ErrNotExist
}

func ignoreFile(f *drive.File) bool {
	return f.Trashed
}

func normalizePath(p string) string {
	return strings.TrimRight(p, "/")
}

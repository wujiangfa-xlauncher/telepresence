package dos

import (
	"context"
	"io"
	"os"
)

// File represents a file in the filesystem. The os.File struct implements this interface
type File interface {
	io.Closer
	io.Reader
	io.ReaderAt
	io.Seeker
	io.Writer
	io.WriterAt

	Name() string
	Readdir(count int) ([]os.FileInfo, error)
	Readdirnames(n int) ([]string, error)
	Stat() (os.FileInfo, error)
	Sync() error
	Truncate(size int64) error
	WriteString(s string) (ret int, err error)
	ReadDir(count int) ([]os.DirEntry, error)
}

// FileSystem is an interface that implements functions in the os package
type FileSystem interface {
	Create(name string) (File, error)
	MkdirAll(path string, perm os.FileMode) error
	Open(name string) (File, error)
	OpenFile(name string, flag int, perm os.FileMode) (File, error)
	Stat(name string) (os.FileInfo, error)
	Symlink(oldName, newName string) error
}

type osFs struct{}

func (osFs) Create(name string) (File, error) {
	return os.Create(name)
}

func (osFs) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (osFs) Open(name string) (File, error) {
	return os.Open(name)
}

func (osFs) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	return os.OpenFile(name, flag, perm)
}

func (osFs) Stat(name string) (os.FileInfo, error) {
	return os.Stat(name)
}

func (osFs) Symlink(oldName, newName string) error {
	return os.Symlink(oldName, newName)
}

type fsKey struct{}

func WithFS(ctx context.Context, fs FileSystem) context.Context {
	return context.WithValue(ctx, fsKey{}, fs)
}

func getFS(ctx context.Context) FileSystem {
	if fs, ok := ctx.Value(fsKey{}).(FileSystem); ok {
		return fs
	}
	return osFs{}
}

func Create(ctx context.Context, name string) (File, error) {
	return getFS(ctx).Create(name)
}

func MkdirAll(ctx context.Context, path string, perm os.FileMode) error {
	return getFS(ctx).MkdirAll(path, perm)
}

func Open(ctx context.Context, name string) (File, error) {
	return getFS(ctx).Open(name)
}

func OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (File, error) {
	return getFS(ctx).OpenFile(name, flag, perm)
}

func Stat(ctx context.Context, name string) (os.FileInfo, error) {
	return getFS(ctx).Stat(name)
}

func Symlink(ctx context.Context, oldName, newName string) error {
	return getFS(ctx).Symlink(oldName, newName)
}

// ReadFile reads the named file and returns the contents.
// A successful call returns err == nil, not err == EOF.
// Because ReadFile reads the whole file, it does not treat an EOF from Read
// as an error to be reported.
// MODIFIED: This function is a verbatim copy of Golang 1.17.6 os.ReadFile in src/os/file.go,
// MODIFIED: except for lines marked "MODIFIED".
func ReadFile(ctx context.Context, name string) ([]byte, error) { // MODIFIED
	f, err := Open(ctx, name) // MODIFIED
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var size int
	if info, err := f.Stat(); err == nil {
		size64 := info.Size()
		if int64(int(size64)) == size64 {
			size = int(size64)
		}
	}
	size++ // one byte for final read at EOF

	// If a file claims a small size, read at least 512 bytes.
	// In particular, files in Linux's /proc claim size 0 but
	// then do not work right if read in small pieces,
	// so an initial read of 1 byte would not work correctly.
	if size < 512 {
		size = 512
	}

	data := make([]byte, 0, size)
	for {
		if len(data) >= cap(data) {
			d := append(data[:cap(data)], 0)
			data = d[:len(data)]
		}
		n, err := f.Read(data[len(data):cap(data)])
		data = data[:len(data)+n]
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return data, err
		}
	}
}

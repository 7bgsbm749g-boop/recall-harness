package primitives

import (
	"context"
	"errors"
	"fmt"
	"os"
)

func createNewPath(ctx context.Context, request IOCreateRequest) (IOCreateResult, error) {
	if err := ctx.Err(); err != nil {
		return IOCreateResult{}, err
	}
	var err error
	if request.Kind == IOCreateDirectory {
		err = os.Mkdir(request.Path, request.Mode)
	} else if request.Kind == IOCreateRegularFile {
		var file *os.File
		file, err = os.OpenFile(request.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, request.Mode)
		if err == nil {
			err = errors.Join(file.Sync(), file.Close())
		}
	} else {
		return IOCreateResult{}, fmt.Errorf("unsupported create kind %d", request.Kind)
	}
	if errors.Is(err, os.ErrExist) {
		info, statErr := os.Lstat(request.Path)
		if statErr != nil {
			return IOCreateResult{}, statErr
		}
		if request.Kind == IOCreateDirectory && !info.IsDir() || request.Kind == IOCreateRegularFile && !info.Mode().IsRegular() {
			return IOCreateResult{}, errors.New("existing path has the wrong kind")
		}
		err = nil
	}
	if err != nil {
		return IOCreateResult{}, err
	}
	return IOCreateResult{Kind: request.Kind}, nil
}

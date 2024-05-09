package ingest

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
)

func createCacheName(collection, signature, source string) string {
	ext := filepath.Ext(source)
	nameBytes := sha256.Sum256([]byte(collection + "/" + signature + "/item"))
	return fmt.Sprintf("%x%s", nameBytes, ext)
}

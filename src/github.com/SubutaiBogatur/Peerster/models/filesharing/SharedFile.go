package filesharing

import (
	"crypto/sha256"
	"encoding/hex"
	. "github.com/SubutaiBogatur/Peerster/config"
	. "github.com/SubutaiBogatur/Peerster/models"
	. "github.com/SubutaiBogatur/Peerster/utils"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// deprecated, use merkleSharedFile
type sharedFile struct {
	// chunks by itself are stored in _SharedFiles/{Name}/{Hash as hex string}.chunk on disk

	Name string
	Size int // in bytes, not bigger then 2 * 1024 * 1024

	MetaHash  [32]byte
	MetaSlice []byte            // stores merged hashes of chunks in right order
	MetaSet   map[[32]byte]bool // stores hashes of chunks
}

func shareFile(path string) *sharedFile {
	path, err := filepath.Abs(path)
	if CheckErr(err) {
		return nil
	}

	if stat, err := os.Stat(path); os.IsNotExist(err) || stat.IsDir() {
		log.Error("file to share doesn't exist")
		return nil
	}

	if _, err := os.Stat(SharedFilesPath); os.IsNotExist(err) {
		os.Mkdir(SharedFilesPath, FileCommonMode)
	}
	if _, err := os.Stat(SharedFilesChunksPath); os.IsNotExist(err) {
		os.Mkdir(SharedFilesChunksPath, FileCommonMode)
	}

	sharedFile := sharedFile{Name: filepath.Base(path)}

	chunksPath := filepath.Join(SharedFilesChunksPath, sharedFile.Name)
	if _, err := os.Stat(chunksPath); !os.IsNotExist(err) {
		return nil // it seems like this sharedFile is already being shared
	}

	os.Mkdir(chunksPath, FileCommonMode)

	// time to read the file and create chunks
	fileBytes, err := ioutil.ReadFile(path)
	if CheckErr(err) {
		return nil
	}

	sharedFile.Size = len(fileBytes)
	if sharedFile.Size > MaxFileSize {
		log.Warn("file was not read, because it exceedes maximum allowed length!")
		return nil
	}

	// split in chunks
	chunks := make([][]byte, 0, len(fileBytes)/FileChunkSize+1)
	var curChunk = make([]byte, 0, FileChunkSize)
	for i := 0; i < len(fileBytes); i++ {
		curChunk = append(curChunk, fileBytes[i])
		if (i+1)%FileChunkSize == 0 || (i+1) == len(fileBytes) {
			chunks = append(chunks, curChunk)
			curChunk = make([]byte, 0, FileChunkSize)
		}
	}

	sharedFile.MetaSlice = make([]byte, 0, len(chunks)*32)
	sharedFile.MetaSet = make(map[[32]byte]bool)
	for _, chunk := range chunks {
		chunkHash := sha256.Sum256(chunk)
		sharedFile.MetaSlice = append(sharedFile.MetaSlice, chunkHash[:]...)
		// it's an interesting case if file has a series of same chunks. Then we still do the sharing, but thanks to metafile downloading side will just request same chunk a few times
		sharedFile.MetaSet[chunkHash] = true
		ioutil.WriteFile(filepath.Join(chunksPath, GetChunkFileName(chunkHash)), chunk, FileCommonMode)
		// now chunk won't be stored in ram
	}

	sharedFile.MetaHash = sha256.Sum256(sharedFile.MetaSlice)
	ioutil.WriteFile(filepath.Join(chunksPath, hex.EncodeToString(sharedFile.MetaHash[:]))+".metafile", sharedFile.MetaSlice, FileCommonMode)

	return &sharedFile
}

func (sf *sharedFile) chunkBelongsToFile(chunkHash [32]byte) bool {
	_, ok := sf.MetaSet[chunkHash]
	return ok
}

func (sf *sharedFile) getChunk(hashValue [32]byte) []byte {
	if !sf.chunkBelongsToFile(hashValue) {
		return nil
	}

	chunkPath := filepath.Join(SharedFilesChunksPath, sf.Name, GetChunkFileName(hashValue))
	if _, err := os.Stat(chunkPath); os.IsNotExist(err) {
		log.Error("existing chunk cannot be found!!!")
		return nil
	}

	chunkBytes, err := ioutil.ReadFile(chunkPath)
	if CheckErr(err) {
		return nil
	}

	return chunkBytes
}

func (sf *sharedFile) getSearchResults(keywords []string) []*SearchResult {
	searchResults := make([]*SearchResult, 0)

	for _, kw := range keywords {
		if res, err := regexp.MatchString(".*"+kw+".*", sf.Name); err == nil && res {
			log.Info("shared file " + sf.Name + " matches search request " + strings.Join(keywords, ","))
			searchResult := &SearchResult{FileName: sf.Name, MetafileHash: sf.MetaHash[:], ChunkCount: uint64(len(sf.MetaSet))}
			chunkMap := make([]uint64, len(sf.MetaSet))
			for i := 1; i <= len(sf.MetaSet); i++ {
				chunkMap[i-1] = uint64(i)
			}
			searchResult.ChunkMap = chunkMap

			searchResults = append(searchResults, searchResult)
		}
	}

	return searchResults
}

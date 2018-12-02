package filesharing

import (
	"crypto/sha256"
	"fmt"
	. "github.com/SubutaiBogatur/Peerster/models"
	. "github.com/SubutaiBogatur/Peerster/utils"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
)

type DownloadingFile struct {
	Name string

	MetaHash [32]byte
	Metafile []byte

	ChunksHashesSlice [][32]byte        // slice of all hashes of all chunks, used to store the order of chunks to rebuild the file, just parsed Metafile in fact
	ChunksHashesSet   map[[32]byte]bool // same as slice, but set. Slice stores order, set provides O(1) access

	ChunksToDownload map[[32]byte]bool // chunk_hash -> bool, is like ChunkHashesSet, but is modified with every new downloaded chunk
}

func InitDownloadingFile(name string, metahash [32]byte) *DownloadingFile {
	return &DownloadingFile{Name: name, MetaHash: metahash} // all others nil
}

func (df *DownloadingFile) fileHasDownloadedChunk(hashValue [32]byte) bool {
	if df.Metafile == nil {
		return false // nothing is downloaded yet
	}

	_, isCorrectChunk := df.ChunksHashesSet[hashValue]
	_, isNotDownloaded := df.ChunksToDownload[hashValue]

	return df.MetaHash == hashValue || (isCorrectChunk && !isNotDownloaded)
}

func (df *DownloadingFile) getChunkOrMetafile(hashValue [32]byte) []byte {
	if hashValue == df.MetaHash {
		log.Info("returned metafile for downloading file")
		return df.Metafile
	}

	if df.fileHasDownloadedChunk(hashValue) {
		chunkPath := filepath.Join(DownloadsChunksPath, df.Name, GetChunkFileName(hashValue))
		if _, err := os.Stat(chunkPath); os.IsNotExist(err) {
			log.Error("existing chunk cannot be found!!!")
			return nil
		}

		chunkBytes, err := ioutil.ReadFile(chunkPath)
		if CheckErr(err) {
			return nil
		}

		log.Info("returned chunk of downloading file")
		return chunkBytes
	}

	// else requesting what's either not downloaded yet or no present at all
	return nil
}

//returns true if downloading is finished, nil if error
func (df *DownloadingFile) ProcessDataReply(drpmsg *DataReply) *bool {
	typedHashValue, err := GetTypeStrictHash(drpmsg.HashValue)
	if CheckErr(err) {
		return nil
	}

	data := drpmsg.Data
	if sha256.Sum256(data) != typedHashValue {
		log.Error("got error hash-value pair!")
		return nil
	}

	// if we received metafile:
	if df.Metafile == nil {
		df.gotMetafile(typedHashValue, data)
		fmt.Println("DOWNLOADING metafile of " + df.Name + " from " + drpmsg.Origin)
		return new(bool) // ptr to false
	}
	// else this is not metahash:

	if _, ok := df.ChunksToDownload[typedHashValue]; !ok {
		log.Warn("got the chunk, which is not in metafile or was already downloaded")
		return nil
	}

	// we got the chunk we were waiting for:
	delete(df.ChunksToDownload, typedHashValue)
	fmt.Println("DOWNLOADING " + df.Name + " chunk " + strconv.Itoa(len(df.ChunksHashesSlice)-len(df.ChunksToDownload)) + " from " + drpmsg.Origin)
	chunkFileName := GetChunkFileName(typedHashValue)
	ioutil.WriteFile(filepath.Join(DownloadsChunksPath, df.Name, chunkFileName), data, FileCommonMode)

	if len(df.ChunksToDownload) == 0 {
		df.finishDownloading()
		a := true
		return &a // even if errors occured, they seem not-repairable
	}

	return new(bool)
}

func (df *DownloadingFile) gotMetafile(hashValue [32]byte, metafile []byte) {
	if len(metafile)%32 != 0 || hashValue != df.MetaHash {
		log.Error("we should have received metafile, but data seems not valid..")
		return
	}

	df.Metafile = make([]byte, len(metafile)) // very strange bug if no copying is done
	copy(df.Metafile, metafile)

	// parse metafile to store it in inner structures:
	df.ChunksHashesSlice = make([][32]byte, 0, len(metafile)/32)
	df.ChunksHashesSet = make(map[[32]byte]bool)
	df.ChunksToDownload = make(map[[32]byte]bool)
	var currentChunk [32]byte
	for i := 0; i < len(metafile); i++ {
		currentChunk[i%32] = metafile[i]
		if (i+1)%32 == 0 {
			df.ChunksHashesSlice = append(df.ChunksHashesSlice, currentChunk)
			df.ChunksHashesSet[currentChunk] = true
			df.ChunksToDownload[currentChunk] = true
		}
	}

	// clean tmp storage:
	if _, err := os.Stat(DownloadsPath); os.IsNotExist(err) {
		os.Mkdir(DownloadsPath, FileCommonMode)
	}
	if _, err := os.Stat(DownloadsChunksPath); os.IsNotExist(err) {
		os.Mkdir(DownloadsChunksPath, FileCommonMode)
	}
	fileChunksPath := filepath.Join(DownloadsChunksPath, df.Name)
	if _, err := os.Stat(fileChunksPath); !os.IsNotExist(err) {
		log.Warn("received metafile for file, which downloading is currently in progress..")
		return
	}

	os.Mkdir(fileChunksPath, FileCommonMode)

	// save metafile to disk:
	metafileName := GetChunkFileName(df.MetaHash)
	ioutil.WriteFile(filepath.Join(fileChunksPath, metafileName), metafile, FileCommonMode)
}

func (df *DownloadingFile) finishDownloading() {
	log.Info("file " + df.Name + " is downloaded, composing it..")
	fileBytes := make([]byte, 0, len(df.ChunksHashesSlice)*FileChunkSize)
	for _, chunkHash := range df.ChunksHashesSlice {
		chunkFileName := GetChunkFileName(chunkHash)
		chunkPath := filepath.Join(DownloadsChunksPath, df.Name, chunkFileName)
		if _, err := os.Stat(chunkPath); os.IsNotExist(err) {
			log.Error("existing chunk cannot be found!!!")
			return
		}

		chunkBytes, err := ioutil.ReadFile(chunkPath)
		if CheckErr(err) {
			log.Error("error, when reading chunk from file")
			return
		}

		fileBytes = append(fileBytes, chunkBytes...)
	}

	log.Info("file composed and being written to persistent memory")
	if _, err := os.Stat(filepath.Join(DownloadsPath, df.Name)); !os.IsNotExist(err) {
		log.Warn("such file alreaqy exists in downloads dir, deleting old file, sorry..")
		os.Remove(filepath.Join(DownloadsPath, df.Name))
	}
	ioutil.WriteFile(filepath.Join(DownloadsPath, df.Name), fileBytes, FileCommonMode)

	log.Debug("file composed & everything is ok, now providing chunks only for sharing")
}

func (df *DownloadingFile) getDataRequest() []byte {
	for k := range df.ChunksToDownload {
		return k[:]
	}

	// everything is downloaded already
	return nil
}

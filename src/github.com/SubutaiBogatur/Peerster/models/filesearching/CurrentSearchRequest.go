package filesearching

import (
	"encoding/hex"
	. "github.com/SubutaiBogatur/Peerster/models"
	. "github.com/SubutaiBogatur/Peerster/utils/send-utils"
	log "github.com/sirupsen/logrus"
	"sync"
)

// accessed from message-processor, search-request goroutine & from webserver, is hard-synchronized
type CurrentSearchRequest struct {
	ch          chan *SearchReply
	fullMatches []*FullSearchMatch
	isAlive     bool

	gossiperUIPort int
	l              *log.Entry // logger

	mux sync.Mutex
}

func InitCurrentSearchRequest(ch chan *SearchReply, gossiperUIPort int, l *log.Entry) *CurrentSearchRequest {
	return &CurrentSearchRequest{ch: ch, fullMatches: make([]*FullSearchMatch, 0), isAlive: true, gossiperUIPort: gossiperUIPort, l: l}
}

func (csr *CurrentSearchRequest) ForwardSearchReply(srp *SearchReply) {
	csr.mux.Lock()
	defer csr.mux.Unlock()

	if !csr.isAlive {
		csr.l.Info("search request is already not in progress, dropping reply")
		return
	}

	csr.ch <- srp
}

func (csr *CurrentSearchRequest) IsAlive() bool {
	return csr.isAlive
}

func (csr *CurrentSearchRequest) AddFullMatch(match *FullSearchMatch) {
	csr.mux.Lock()
	defer csr.mux.Unlock()

	// O(n), but, once again, not the worst problem
	for _, otherMatch := range csr.fullMatches {
		if otherMatch.Origin == match.Origin && otherMatch.MetafileHash == match.MetafileHash {
			csr.l.Warn("got match, but already have this match, so not really counting it")
			return
		}
	}
	// else if truly new
	csr.fullMatches = append(csr.fullMatches, match)
}

func (csr *CurrentSearchRequest) GetFullMatchesNumber() int {
	csr.mux.Lock()
	defer csr.mux.Unlock()

	return len(csr.fullMatches)
}

func (csr *CurrentSearchRequest) DownloadFileByHash(metahash [32]byte, downloadFilename string) {
	csr.mux.Lock()
	defer csr.mux.Unlock()

	for _, match := range csr.fullMatches {
		if match.MetafileHash == metahash {
			// great, found full match with required file
			csr.l.Info("good, initiating specific downloading of " + downloadFilename + " from " + match.Origin + " for hash " + hex.EncodeToString(metahash[:]))
			SendToDownloadMessageToLocalPort(downloadFilename, hex.EncodeToString(match.MetafileHash[:]), match.Origin, csr.gossiperUIPort, csr.l)
			return
		}
	}

	csr.l.Error("couldn't initiate any downloading because search request got no full matches")
}

func (csr *CurrentSearchRequest) DownloadFileByName(filename string, downloadFilename string) {
	csr.mux.Lock()
	defer csr.mux.Unlock()

	for _, match := range csr.fullMatches {
		if match.Filename == filename {
			// great, found full match with required file
			csr.l.Info("good, initiating specific downloading of " + filename + " from " + match.Origin)
			SendToDownloadMessageToLocalPort(downloadFilename, hex.EncodeToString(match.MetafileHash[:]), match.Origin, csr.gossiperUIPort, csr.l)
			return
		}
	}

	csr.l.Error("couldn't initiate specific downloading of " + filename + " because search request got no full matches")
}

func (csr *CurrentSearchRequest) Shutdown() {
	csr.mux.Lock()
	defer csr.mux.Unlock()

	csr.isAlive = false
}

func (csr *CurrentSearchRequest) GetFullMatches() []string {
	csr.mux.Lock()
	defer csr.mux.Unlock()

	ret := make([]string, 0)
	for _, match := range csr.fullMatches {
		ret = append(ret, match.Filename)
	}

	return ret
}

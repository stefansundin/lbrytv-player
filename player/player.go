package player

import (
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/lbryio/ccache/v2"
	"github.com/lbryio/lbrytv-player/internal/metrics"
	"github.com/lbryio/lbrytv-player/pkg/logger"
	"github.com/lbryio/lbrytv-player/pkg/paid"

	ljsonrpc "github.com/lbryio/lbry.go/v2/extras/jsonrpc"
)

var Logger = logger.GetLogger()

// Player is an entry-point object to the new player package.
type Player struct {
	lbrynetClient *ljsonrpc.Client
	blobSource    *HotCache
	prefetch      bool
	resolveCache  *ccache.Cache
}

// NewPlayer initializes an instance with optional BlobStore.
func NewPlayer(hotCache *HotCache, lbrynetAddress string) *Player {
	if lbrynetAddress == "" {
		lbrynetAddress = "http://localhost:5279"
	}

	return &Player{
		lbrynetClient: ljsonrpc.NewClient(lbrynetAddress),
		blobSource:    hotCache,
		resolveCache:  ccache.New(ccache.Configure().MaxSize(10000)),
	}
}

func (p *Player) SetPrefetch(enabled bool) {
	p.prefetch = enabled
}

// Play delivers requested URI onto the supplied http.ResponseWriter.
func (p *Player) Play(s *Stream, w http.ResponseWriter, r *http.Request) error {
	metrics.StreamsRunning.Inc()
	defer metrics.StreamsRunning.Dec()
	ServeStream(w, r, s)
	return nil
}

// ResolveStream resolves provided URI by calling the SDK.
func (p *Player) ResolveStream(uri string) (*Stream, error) {
	var claim *ljsonrpc.Claim

	cachedClaim := p.resolveCache.Get(uri)
	if cachedClaim != nil && !cachedClaim.Expired() {
		claim = cachedClaim.Value().(*ljsonrpc.Claim)
	} else {
		var err error
		claim, err = p.resolve(uri)
		if err != nil {
			return nil, err
		}
		p.resolveCache.Set(uri, claim, time.Duration(rand.Intn(5)+5)*time.Minute) // random time between 5 and 10 min, to spread load on wallet servers
	}

	return NewStream(p, uri, claim), nil
}

// resolve the uri
func (p *Player) resolve(uri string) (*ljsonrpc.Claim, error) {
	resolved, err := p.lbrynetClient.Resolve(uri)
	if err != nil {
		return nil, err
	}

	claim := (*resolved)[uri]
	if claim.CanonicalURL == "" {
		return nil, errStreamNotFound
	}

	return &claim, nil
}

// VerifyAccess checks if the stream is paid and the token supplied matched the stream
func (p *Player) VerifyAccess(s *Stream, token string) error {
	if s.resolvedStream.Fee == nil || s.resolvedStream.Fee.Amount <= 0 {
		return nil
	}

	Logger.WithField("uri", s.URI).Info("paid stream requested")
	if token == "" {
		return errPaidStream
	}
	if err := paid.VerifyStreamAccess(strings.Replace(s.URI, "#", "/", 1), token); err != nil {
		return err
	}
	return nil
}

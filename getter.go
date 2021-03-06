package s3gof3r

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"time"
)

type getter struct {
	url    url.URL
	client *http.Client
	b      *Bucket
	bufsz  int64
	err    error
	wg     sync.WaitGroup

	cur_chunk_id   int
	cur_chunk      *chunk
	content_length int64
	chunk_total    int
	read_ch        chan *chunk
	get_ch         chan *chunk

	bp *bp

	q_wait map[int]*chunk

	concurrency int
	nTry        int
	closed      bool
	c           *Config

	md5 hash.Hash
}

type chunk struct {
	id     int
	header http.Header
	start  int64
	size   int64
	b      *bytes.Buffer
	len    int64
}

func newGetter(p_url url.URL, c *Config, b *Bucket) (io.ReadCloser, http.Header, error) {
	// initialize getter
	g := new(getter)
	g.url = p_url
	g.bufsz = c.PartSize
	g.get_ch = make(chan *chunk)
	g.read_ch = make(chan *chunk)
	g.nTry = c.NTry
	g.q_wait = make(map[int]*chunk)
	g.b = b
	g.c = c
	g.client = c.Client
	g.md5 = md5.New()

	// get content length
	resp, err := g.retryRequest("HEAD", p_url.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, nil, newRespError(resp)
	}
	g.content_length = resp.ContentLength
	g.chunk_total = int((g.content_length + g.bufsz - 1) / g.bufsz) // round up, integer division
	g.concurrency = min(c.Concurrency, g.chunk_total)
	log.Println("chunk total: ", g.chunk_total)
	log.Println("content length : ", g.content_length)
	log.Println("concurrency: ", g.concurrency)

	g.bp = NewBufferPool(g.bufsz)

	for i := 0; i < g.concurrency; i++ {
		go g.worker()
	}
	go g.init_chunks()
	return g, resp.Header, nil
}

func (g *getter) retryRequest(method, urlStr string, body io.ReadSeeker) (resp *http.Response, err error) {
	for i := 0; i < g.nTry; i++ {
		var req *http.Request
		req, err = http.NewRequest(method, urlStr, body)
		if err != nil {
			return
		}
		g.b.Sign(req)
		resp, err = g.client.Do(req)
		if err == nil {
			return
		}
		log.Println(err)
		if body != nil {
			body.Seek(0, 0)
		}
	}
	return
}

func (g *getter) init_chunks() {
	id := 0
	for i := int64(0); i < g.content_length; {
		for len(g.q_wait) > g.concurrency {
			// Limit growth of q_wait
			timeout := time.NewTimer(time.Second)
			<-timeout.C
		}
		size := min64(g.bufsz, g.content_length-i)
		c := &chunk{
			id: id,
			header: http.Header{
				"Range": {fmt.Sprintf("bytes=%d-%d",
					i, i+size-1)},
			},
			start: i,
			size:  size,
			b:     nil,
			len:   0}
		i += size
		id++
		g.wg.Add(1)
		g.get_ch <- c
	}
	close(g.get_ch)
}

func (g *getter) worker() {
	for c := range g.get_ch {
		g.retryGetChunk(c)
	}

}

func (g *getter) retryGetChunk(c *chunk) {
	defer g.wg.Done()
	var err error
	c.b = <-g.bp.get
	for i := 0; i < g.nTry; i++ {
		err = g.getChunk(c)
		if err == nil {
			return
		}
		log.Printf("Error on attempt %d: retrying chunk: %v, Error: %s", i, c, err)
	}
	g.err = err
}

func (g *getter) getChunk(c *chunk) error {
	// ensure buffer is empty
	c.b.Reset()

	r, err := http.NewRequest("GET", g.url.String(), nil)
	if err != nil {
		return err
	}
	r.Header = c.header
	g.b.Sign(r)
	resp, err := g.client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 206 {
		return newRespError(resp)
	}
	n, err := c.b.ReadFrom(resp.Body)
	if err != nil {
		return err
	}
	if n != c.size {
		return fmt.Errorf("Chunk %d: Expected %d bytes, received %d",
			c.id, c.size, n)
	}
	g.read_ch <- c
	return nil
}

func (g *getter) Read(p []byte) (int, error) {
	var err error
	if g.closed {
		return 0, syscall.EINVAL
	}
	if g.err != nil {
		return 0, g.err
	}
	if g.cur_chunk == nil {
		g.cur_chunk = g.find_next_chunk()
	}
	// write to md5 hash in parallel with output
	tr := io.TeeReader(g.cur_chunk.b, g.md5)
	n, err := tr.Read(p)

	// Empty buffer, move on to next
	if err == io.EOF {
		// Do not send EOF for each chunk.
		if g.cur_chunk.id == g.chunk_total-1 && g.cur_chunk.b.Len() == 0 {
			return n, err // end of stream, send eof
		}
		g.bp.give <- g.cur_chunk.b // recycle buffer
		g.cur_chunk = nil
		g.cur_chunk_id++
		return n - 1, nil // subtract EOF
	}
	return n, err
}

func (g *getter) find_next_chunk() (cur_chunk *chunk) {
	for {
		// first check q_wait
		if cur_chunk, ok := g.q_wait[g.cur_chunk_id]; ok {
			delete(g.q_wait, g.cur_chunk_id)
			return cur_chunk
		}
		// if next chunk not in q_wait, read from channel
		c := <-g.read_ch
		g.q_wait[c.id] = c
	}
}

func (g *getter) Close() error {
	if g.closed {
		return syscall.EINVAL
	}
	if g.err != nil {
		return g.err
	}
	g.wg.Wait()
	close(g.read_ch)
	g.bp.quit <- true
	g.closed = true
	log.Println("makes:", g.bp.makes)
	if g.c.Md5Check {
		if err := g.checkMd5(); err != nil {
			return err
		}
	}

	return nil
}

func (g *getter) checkMd5() (err error) {
	calcMd5 := fmt.Sprintf("%x", g.md5.Sum(nil))
	md5Path := fmt.Sprint(".md5", g.url.Path, ".md5")
	md5Url := g.b.Url(md5Path, g.c)
	log.Println("md5: ", calcMd5)
	log.Println("md5Path: ", md5Path)
	resp, err := g.retryRequest("GET", md5Url.String(), nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("MD5 check failed: %s not found: %s", md5Url.String(), newRespError(resp))
	}
	givenMd5, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}
	if calcMd5 != string(givenMd5) {
		return fmt.Errorf("MD5 mismatch. given:%s calculated:%s", givenMd5, calcMd5)
	}
	return
}

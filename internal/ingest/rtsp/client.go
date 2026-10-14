package rtsp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// rtspClient is a minimal RTSP 1.0 client that pulls media over a single TCP
// connection using interleaved RTP (RFC 2326 §10.12).
type rtspClient struct {
	conn    net.Conn
	br      *bufio.Reader
	cseq    int
	session string
	rtpChan int
}

type rtspResponse struct {
	status  int
	headers map[string]string
	body    string
}

func dialRTSP(rawURL string, timeout time.Duration) (*rtspClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("rtsp: parse url: %w", err)
	}
	if u.Scheme != "rtsp" && u.Scheme != "rtsps" {
		return nil, errors.New("rtsp: unsupported scheme " + u.Scheme)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "554")
	}
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, fmt.Errorf("rtsp: dial %s: %w", host, err)
	}
	return &rtspClient{conn: conn, br: bufio.NewReader(conn)}, nil
}

// setup performs OPTIONS/DESCRIBE/SETUP/PLAY and returns the video description.
func (c *rtspClient) setup(rawURL string) (*mediaDesc, error) {
	resp, err := c.do("OPTIONS", rawURL, nil)
	if err != nil {
		return nil, err
	}
	if resp.status >= 400 {
		return nil, fmt.Errorf("rtsp: OPTIONS status %d", resp.status)
	}

	desc, err := c.describe(rawURL)
	if err != nil {
		return nil, err
	}

	setupURL := resolveControl(rawURL, desc.control)
	resp, err = c.do("SETUP", setupURL, map[string]string{
		"Transport": "RTP/AVP/TCP;unicast;interleaved=0-1",
	})
	if err != nil {
		return nil, err
	}
	if resp.status >= 400 {
		return nil, fmt.Errorf("rtsp: SETUP status %d", resp.status)
	}
	if s := resp.headers["session"]; s != "" {
		c.session = strings.TrimSpace(strings.SplitN(s, ";", 2)[0])
	}
	c.rtpChan = parseInterleaved(resp.headers["transport"])

	resp, err = c.do("PLAY", rawURL, nil)
	if err != nil {
		return nil, err
	}
	if resp.status >= 400 {
		return nil, fmt.Errorf("rtsp: PLAY status %d", resp.status)
	}
	return desc, nil
}

func (c *rtspClient) describe(rawURL string) (*mediaDesc, error) {
	resp, err := c.do("DESCRIBE", rawURL, map[string]string{"Accept": "application/sdp"})
	if err != nil {
		return nil, err
	}
	if resp.status >= 400 {
		return nil, fmt.Errorf("rtsp: DESCRIBE status %d", resp.status)
	}
	return parseSDP(resp.body)
}

// do writes a request and reads its response. It must only be used before PLAY
// (when no interleaved frames can arrive).
func (c *rtspClient) do(method, reqURL string, headers map[string]string) (*rtspResponse, error) {
	if err := c.writeRequest(method, reqURL, headers); err != nil {
		return nil, err
	}
	return c.readResponse()
}

func (c *rtspClient) writeRequest(method, reqURL string, headers map[string]string) error {
	c.cseq++
	var b strings.Builder
	b.WriteString(method + " " + reqURL + " RTSP/1.0\r\n")
	b.WriteString("CSeq: " + strconv.Itoa(c.cseq) + "\r\n")
	b.WriteString("User-Agent: ari-media-engine\r\n")
	for k, v := range headers {
		b.WriteString(k + ": " + v + "\r\n")
	}
	if c.session != "" {
		b.WriteString("Session: " + c.session + "\r\n")
	}
	b.WriteString("\r\n")
	_, err := io.WriteString(c.conn, b.String())
	return err
}

func (c *rtspClient) readResponse() (*rtspResponse, error) {
	line, err := c.br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "RTSP/") {
		return nil, errors.New("rtsp: unexpected response: " + line)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return nil, errors.New("rtsp: malformed status line: " + line)
	}
	status, _ := strconv.Atoi(fields[1])

	headers, err := c.readHeaders()
	if err != nil {
		return nil, err
	}
	body, err := c.readBody(headers)
	if err != nil {
		return nil, err
	}
	return &rtspResponse{status: status, headers: headers, body: body}, nil
}

func (c *rtspClient) readHeaders() (map[string]string, error) {
	headers := map[string]string{}
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return headers, nil
		}
		if i := strings.IndexByte(line, ':'); i > 0 {
			headers[strings.ToLower(strings.TrimSpace(line[:i]))] = strings.TrimSpace(line[i+1:])
		}
	}
}

func (c *rtspClient) readBody(headers map[string]string) (string, error) {
	cl := headers["content-length"]
	if cl == "" {
		return "", nil
	}
	n, err := strconv.Atoi(cl)
	if err != nil || n <= 0 {
		return "", nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.br, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// readLoop dispatches interleaved RTP frames and consumes stray RTSP responses
// (e.g. keep-alive replies) until the connection ends.
func (c *rtspClient) readLoop(onRTP func(payload []byte)) error {
	for {
		b, err := c.br.ReadByte()
		if err != nil {
			return err
		}
		if b == '$' {
			var hdr [3]byte
			if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
				return err
			}
			channel := int(hdr[0])
			length := int(hdr[1])<<8 | int(hdr[2])
			payload := make([]byte, length)
			if _, err := io.ReadFull(c.br, payload); err != nil {
				return err
			}
			if channel == c.rtpChan && length > 0 {
				onRTP(payload)
			}
			continue
		}
		if err := c.consumeResponse(b); err != nil {
			return err
		}
	}
}

func (c *rtspClient) consumeResponse(first byte) error {
	line, err := c.br.ReadString('\n')
	if err != nil {
		return err
	}
	_ = string(first) + line
	headers, err := c.readHeaders()
	if err != nil {
		return err
	}
	_, err = c.readBody(headers)
	return err
}

// keepAlive writes an OPTIONS request; its response is consumed by readLoop.
func (c *rtspClient) keepAlive(rawURL string) error {
	return c.writeRequest("OPTIONS", rawURL, nil)
}

func (c *rtspClient) close() { _ = c.conn.Close() }

// parseInterleaved returns the RTP channel from a Transport header
// (e.g. "RTP/AVP/TCP;unicast;interleaved=0-1" -> 0).
func parseInterleaved(transport string) int {
	for _, part := range strings.Split(transport, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "interleaved=") {
			val := strings.TrimPrefix(part, "interleaved=")
			if i := strings.IndexByte(val, '-'); i > 0 {
				val = val[:i]
			}
			if n, err := strconv.Atoi(val); err == nil {
				return n
			}
		}
	}
	return 0
}

package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
	"unicode"
)

const pingTimeout = 4 * time.Second

// motdSpan is one run of MOTD text sharing the same colour/formatting, ready for
// the UI to render as a styled <span> (the same way Minecraft draws the MOTD).
type motdSpan struct {
	Text          string `json:"text"`
	Color         string `json:"color,omitempty"` // hex, e.g. "#55ff55"
	Bold          bool   `json:"bold,omitempty"`
	Italic        bool   `json:"italic,omitempty"`
	Underline     bool   `json:"underline,omitempty"`
	Strikethrough bool   `json:"strike,omitempty"`
	Obfuscated    bool   `json:"obf,omitempty"`
}

// serverStatus is the parsed result of a Server List Ping: latency plus the same
// bits Minecraft shows for a server (MOTD, player counts, version, icon).
type serverStatus struct {
	Ms         int        `json:"ms"`
	Motd       []motdSpan `json:"motd,omitempty"`
	PlayersOn  int        `json:"playersOnline"`
	PlayersMax int        `json:"playersMax"`
	Version    string     `json:"version,omitempty"`
	Favicon    string     `json:"favicon,omitempty"` // data: URI PNG
}

// pingServer performs a Minecraft Server List Ping (the same handshake the
// multiplayer menu uses) against addr and returns the server's status: the
// ping/pong round-trip in milliseconds plus the MOTD, player counts, version and
// icon. addr is our local cloudflared listener, so the request travels the full
// tunnel to the real server - i.e. the true latency the player feels.
func pingServer(addr string) (serverStatus, error) {
	var res serverStatus
	conn, err := net.DialTimeout("tcp", addr, pingTimeout)
	if err != nil {
		return res, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(pingTimeout))

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return res, err
	}
	var port uint16 = 25565
	fmt.Sscanf(portStr, "%d", &port)

	r := bufio.NewReader(conn)

	// Handshake: protocol version, address, port, next state = 1 (status). We send
	// a modern protocol (1.21.x) rather than an old one so the server serialises
	// the MOTD with full hex colours instead of downsampling a hex gradient to the
	// nearest legacy 16-colour palette.
	hs := newPacket(0x00)
	hs.varInt(767)
	hs.str(host)
	hs.uShort(port)
	hs.varInt(1)
	if err := hs.send(conn); err != nil {
		return res, err
	}

	// Status request, then read and parse the status JSON response.
	if err := newPacket(0x00).send(conn); err != nil {
		return res, err
	}
	body, err := readPacketBody(r)
	if err != nil {
		return res, err
	}
	if s, err := parseStatusJSON(body); err == nil {
		res = s
	}

	// Ping with a payload; the server echoes it back as Pong. Time that.
	start := time.Now()
	pp := newPacket(0x01)
	pp.long(start.UnixNano())
	if err := pp.send(conn); err != nil {
		return res, err
	}
	if err := discardPacket(r); err != nil {
		return res, err
	}
	res.Ms = int(time.Since(start).Milliseconds())
	return res, nil
}

// parseStatusJSON decodes a status-response packet body (packet id + JSON string)
// into a serverStatus, pulling out the MOTD, players, version and favicon.
func parseStatusJSON(body []byte) (serverStatus, error) {
	var res serverStatus
	br := bufio.NewReader(bytes.NewReader(body))
	if _, err := readVarInt(br); err != nil { // packet id (0x00)
		return res, err
	}
	strLen, err := readVarInt(br)
	if err != nil {
		return res, err
	}
	if strLen < 0 || int(strLen) > len(body) {
		return res, fmt.Errorf("bad status string length %d", strLen)
	}
	buf := make([]byte, strLen)
	if _, err := io.ReadFull(br, buf); err != nil {
		return res, err
	}

	var raw struct {
		Description json.RawMessage `json:"description"`
		Players     struct {
			Max    int `json:"max"`
			Online int `json:"online"`
		} `json:"players"`
		Version struct {
			Name string `json:"name"`
		} `json:"version"`
		Favicon string `json:"favicon"`
	}
	if err := json.Unmarshal(buf, &raw); err != nil {
		return res, err
	}
	res.PlayersMax = raw.Players.Max
	res.PlayersOn = raw.Players.Online
	res.Version = strings.TrimSpace(raw.Version.Name)
	if fav := strings.TrimSpace(raw.Favicon); strings.HasPrefix(fav, "data:image/") {
		res.Favicon = fav
	}
	res.Motd = parseChat(raw.Description, chatStyle{})
	return res, nil
}

// ----- Minecraft chat / MOTD parsing ----------------------------------------

// chatStyle is the formatting in effect at a point in the MOTD, inherited from a
// parent chat component and mutated by legacy (§) codes as we scan text.
type chatStyle struct {
	color                                string
	bold, italic, underline, strike, obf bool
}

// legacyColors maps the 16 legacy §-code colours to their hex values.
var legacyColors = map[rune]string{
	'0': "#000000", '1': "#0000aa", '2': "#00aa00", '3': "#00aaaa",
	'4': "#aa0000", '5': "#aa00aa", '6': "#ffaa00", '7': "#aaaaaa",
	'8': "#555555", '9': "#5555ff", 'a': "#55ff55", 'b': "#55ffff",
	'c': "#ff5555", 'd': "#ff55ff", 'e': "#ffff55", 'f': "#ffffff",
}

// namedColors maps modern chat-component colour names to hex.
var namedColors = map[string]string{
	"black": "#000000", "dark_blue": "#0000aa", "dark_green": "#00aa00",
	"dark_aqua": "#00aaaa", "dark_red": "#aa0000", "dark_purple": "#aa00aa",
	"gold": "#ffaa00", "gray": "#aaaaaa", "dark_gray": "#555555",
	"blue": "#5555ff", "green": "#55ff55", "aqua": "#55ffff",
	"red": "#ff5555", "light_purple": "#ff55ff", "yellow": "#ffff55",
	"white": "#ffffff",
}

// parseChat turns a chat component (a JSON string, object, or array - the shapes
// a server's "description" can take) into a flat list of styled spans, carrying
// the inherited style down into children.
func parseChat(raw json.RawMessage, st chatStyle) []motdSpan {
	if len(raw) == 0 {
		return nil
	}
	// String form (may itself embed legacy § codes).
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return parseLegacy(s, st)
	}
	// Array form: a sequence of components, each inheriting the same base style.
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		var out []motdSpan
		for _, el := range arr {
			out = append(out, parseChat(el, st)...)
		}
		return out
	}
	// Object form.
	var obj struct {
		Text          string            `json:"text"`
		Color         string            `json:"color"`
		Bold          *bool             `json:"bold"`
		Italic        *bool             `json:"italic"`
		Underlined    *bool             `json:"underlined"`
		Strikethrough *bool             `json:"strikethrough"`
		Obfuscated    *bool             `json:"obfuscated"`
		Extra         []json.RawMessage `json:"extra"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	cur := st
	if c := normalizeColor(obj.Color); c != "" {
		cur.color = c
	}
	if obj.Bold != nil {
		cur.bold = *obj.Bold
	}
	if obj.Italic != nil {
		cur.italic = *obj.Italic
	}
	if obj.Underlined != nil {
		cur.underline = *obj.Underlined
	}
	if obj.Strikethrough != nil {
		cur.strike = *obj.Strikethrough
	}
	if obj.Obfuscated != nil {
		cur.obf = *obj.Obfuscated
	}
	var out []motdSpan
	if obj.Text != "" {
		out = append(out, parseLegacy(obj.Text, cur)...)
	}
	for _, el := range obj.Extra {
		out = append(out, parseChat(el, cur)...)
	}
	return out
}

// parseLegacy scans a string for legacy formatting codes (§ followed by a colour
// or style character), splitting it into spans. base is the style in effect
// before the string; a reset code (§r) returns to it.
func parseLegacy(s string, base chatStyle) []motdSpan {
	var out []motdSpan
	cur := base
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		out = append(out, spanFrom(cur, b.String()))
		b.Reset()
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '§' && i+1 < len(runes) {
			code := unicode.ToLower(runes[i+1])

			// Bungeecord/Spigot hex colour: §x§R§R§G§G§B§B (how gradients are
			// encoded in a legacy string). Collapse the seven codes into one
			// #rrggbb colour instead of treating each as a separate legacy code.
			if code == 'x' {
				if hex, next, ok := readBungeeHex(runes, i); ok {
					flush()
					cur = chatStyle{color: hex}
					i = next - 1
					continue
				}
			}

			flush()
			applyCode(&cur, base, code)
			i++
			continue
		}
		b.WriteRune(runes[i])
	}
	flush()
	return out
}

// readBungeeHex parses a §x§R§R§G§G§B§B sequence beginning at the §x at index i,
// returning the "#rrggbb" colour and the index just past the sequence.
func readBungeeHex(runes []rune, i int) (string, int, bool) {
	var hex [6]rune
	j := i + 2 // skip "§x"
	for k := 0; k < 6; k++ {
		if j+1 >= len(runes) || runes[j] != '§' {
			return "", 0, false
		}
		d := unicode.ToLower(runes[j+1])
		if !isHexDigit(d) {
			return "", 0, false
		}
		hex[k] = d
		j += 2
	}
	return "#" + string(hex[:]), j, true
}

func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
}

// applyCode mutates cur for a single legacy code character. A colour code resets
// formatting (as Minecraft does); §r returns to the inherited base style.
func applyCode(cur *chatStyle, base chatStyle, code rune) {
	if col, ok := legacyColors[code]; ok {
		*cur = chatStyle{color: col}
		return
	}
	switch code {
	case 'l':
		cur.bold = true
	case 'o':
		cur.italic = true
	case 'n':
		cur.underline = true
	case 'm':
		cur.strike = true
	case 'k':
		cur.obf = true
	case 'r':
		*cur = base
	}
}

// normalizeColor resolves a chat-component colour (a name or "#rrggbb") to hex,
// or "" if it is empty/unrecognised.
func normalizeColor(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	if c == "" {
		return ""
	}
	if strings.HasPrefix(c, "#") {
		return c
	}
	return namedColors[c]
}

func spanFrom(st chatStyle, text string) motdSpan {
	return motdSpan{
		Text:          text,
		Color:         st.color,
		Bold:          st.bold,
		Italic:        st.italic,
		Underline:     st.underline,
		Strikethrough: st.strike,
		Obfuscated:    st.obf,
	}
}

// packet builds a Minecraft protocol packet body (packet id + fields); send()
// prefixes it with its VarInt length.
type packet struct{ buf bytes.Buffer }

func newPacket(id int32) *packet {
	p := &packet{}
	writeVarInt(&p.buf, id)
	return p
}
func (p *packet) varInt(v int32)  { writeVarInt(&p.buf, v) }
func (p *packet) uShort(v uint16) { _ = binary.Write(&p.buf, binary.BigEndian, v) }
func (p *packet) long(v int64)    { _ = binary.Write(&p.buf, binary.BigEndian, v) }
func (p *packet) str(s string) {
	writeVarInt(&p.buf, int32(len(s)))
	p.buf.WriteString(s)
}
func (p *packet) send(conn net.Conn) error {
	var out bytes.Buffer
	writeVarInt(&out, int32(p.buf.Len()))
	out.Write(p.buf.Bytes())
	_, err := conn.Write(out.Bytes())
	return err
}

func writeVarInt(b *bytes.Buffer, v int32) {
	uv := uint32(v)
	for {
		c := byte(uv & 0x7F)
		uv >>= 7
		if uv != 0 {
			c |= 0x80
		}
		b.WriteByte(c)
		if uv == 0 {
			return
		}
	}
}

func readVarInt(r *bufio.Reader) (int32, error) {
	var result int32
	var shift uint
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		result |= int32(b&0x7F) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
		if shift >= 35 {
			return 0, fmt.Errorf("varint too long")
		}
	}
	return result, nil
}

// discardPacket reads one length-prefixed packet and throws away its body.
func discardPacket(r *bufio.Reader) error {
	length, err := readVarInt(r)
	if err != nil {
		return err
	}
	if length < 0 || length > 1<<20 {
		return fmt.Errorf("bad packet length %d", length)
	}
	_, err = io.CopyN(io.Discard, r, int64(length))
	return err
}

// readPacketBody reads one length-prefixed packet and returns its body bytes.
func readPacketBody(r *bufio.Reader) ([]byte, error) {
	length, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	if length < 0 || length > 1<<20 {
		return nil, fmt.Errorf("bad packet length %d", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

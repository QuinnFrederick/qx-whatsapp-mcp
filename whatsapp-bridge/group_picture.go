package main

// group_picture.go — set or remove the picture of a group or community (fork,
// see FORK.md).
//
// Groups and communities store their picture the same way: an iq set in the
// w:profile:picture namespace that targets the @g.us JID, which is exactly
// what whatsmeow's SetGroupPhoto sends. A community's own picture is set on
// the community (parent) JID; each group inside it has its own picture.
// Reading is where communities differ: an account that is not in the parent
// group itself (most members) gets 401 from that namespace and has to use
// the w:g2 query whatsmeow exposes as GetProfilePictureParams.IsCommunity
// (see communityPictureFallback and the /api/profile-picture route).
//
// The server only takes a JPEG, and official clients send a 640x640 square,
// so every JPEG, PNG or WebP input is turned upright (EXIF orientation),
// centre-cropped to a square, scaled to 640x640, flattened onto white and
// re-encoded as JPEG here. Callers can pass any photo as it is.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	_ "image/png" // registers the PNG decoder for image.Decode
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the WebP decoder for image.Decode
)

const (
	// groupPictureSide is the edge of the square JPEG sent to WhatsApp, the
	// size its own clients upload.
	groupPictureSide        = 640
	groupPictureJPEGQuality = 85
	// maxPictureInputBytes caps the image data accepted from any source.
	maxPictureInputBytes = 10 << 20
	// maxPictureRequestBytes leaves room for base64's 4/3 overhead plus the
	// rest of the JSON body.
	maxPictureRequestBytes = maxPictureInputBytes/3*4 + 64<<10
	// maxPicturePixels guards against decompression bombs: a small file can
	// declare enormous dimensions. It is checked before the pixels are decoded.
	maxPicturePixels       = 50_000_000
	pictureDownloadTimeout = 15 * time.Second
	maxPictureRedirects    = 3
)

// errPictureTooLarge marks input over maxPictureInputBytes (reported as 413).
var errPictureTooLarge = fmt.Errorf("the image is larger than %d MB", maxPictureInputBytes>>20)

type groupPictureRequest struct {
	GroupJID    string `json:"group_jid"`
	ImageURL    string `json:"image_url,omitempty"`
	ImagePath   string `json:"image_path,omitempty"`
	ImageBase64 string `json:"image_base64,omitempty"`
	// Remove clears the picture; it takes no image source.
	Remove bool `json:"remove,omitempty"`
}

// source returns the one image input that was given ("image_url",
// "image_path" or "image_base64"), or "remove". Exactly one source is
// required unless remove is set, and remove takes none.
func (r groupPictureRequest) source() (string, error) {
	var given []string
	for _, f := range []struct{ name, value string }{
		{"image_url", r.ImageURL}, {"image_path", r.ImagePath}, {"image_base64", r.ImageBase64},
	} {
		if strings.TrimSpace(f.value) != "" {
			given = append(given, f.name)
		}
	}
	switch {
	case r.Remove && len(given) > 0:
		return "", fmt.Errorf("remove=true takes no image; drop %s", strings.Join(given, ", "))
	case r.Remove:
		return "remove", nil
	case len(given) == 0:
		return "", errors.New("give exactly one of image_url, image_path or image_base64, or remove=true")
	case len(given) > 1:
		return "", fmt.Errorf("give only one image source, got %s", strings.Join(given, " and "))
	}
	return given[0], nil
}

// decodeImageBase64 accepts standard or URL-safe base64, with or without
// padding or line breaks, and a data: URL prefix ("data:image/png;base64,").
func decodeImageBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "data:") {
		comma := strings.IndexByte(s, ',')
		if comma < 0 || !strings.HasSuffix(s[:comma], ";base64") {
			return nil, errors.New("image_base64 data URL must be base64-encoded (data:<type>;base64,...)")
		}
		s = s[comma+1:]
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimRight(s, "=")
	if base64.RawStdEncoding.DecodedLen(len(s)) > maxPictureInputBytes {
		return nil, errPictureTooLarge
	}
	enc := base64.RawStdEncoding
	if strings.ContainsAny(s, "-_") {
		enc = base64.RawURLEncoding
	}
	data, err := enc.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("image_base64 is not valid base64: %v", err)
	}
	if len(data) == 0 {
		return nil, errors.New("image_base64 is empty")
	}
	return data, nil
}

// readImageFile reads an image_path confined to the outbound media roots, the
// same allow-list /api/send enforces (see media_path.go).
func readImageFile(path string, roots []string) ([]byte, error) {
	canonical, err := validateMediaPath(strings.TrimSpace(path), roots)
	if err != nil {
		return nil, errors.New(strings.ReplaceAll(err.Error(), "media_path", "image_path"))
	}
	f, err := os.Open(canonical)
	if err != nil {
		return nil, fmt.Errorf("opening image_path: %v", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxPictureInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading image_path: %v", err)
	}
	if len(data) > maxPictureInputBytes {
		return nil, errPictureTooLarge
	}
	return data, nil
}

// pictureContentTypes are the Content-Types accepted from an image_url.
var pictureContentTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}

// nonPublicPrefixes are ranges netip's predicates do not already cover.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT; also Tailscale
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, incl. broadcast
}

// isPublicAddr reports whether an image_url may be fetched from addr. The
// bridge runs next to local services, so loopback, private, link-local and
// similar addresses are refused (no server-side request forgery).
func isPublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// publicAddrOnly is a net.Dialer Control hook. It runs on the resolved IP
// for every connection, redirects included, so DNS cannot point a public
// hostname at a local service.
func publicAddrOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !isPublicAddr(addr) {
		return fmt.Errorf("image_url points at a non-public address (%s); only public https URLs are allowed", host)
	}
	return nil
}

// httpsRedirectsOnly keeps redirects few and on https.
func httpsRedirectsOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= maxPictureRedirects {
		return fmt.Errorf("image_url redirected more than %d times", maxPictureRedirects)
	}
	if req.URL.Scheme != "https" {
		return errors.New("image_url redirected to a non-https URL")
	}
	return nil
}

// newPictureHTTPClient is the client image_url downloads use in production.
// No proxy: a proxy would make the dial-time address check meaningless.
func newPictureHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: publicAddrOnly}
	return &http.Client{
		Timeout:       pictureDownloadTimeout,
		CheckRedirect: httpsRedirectsOnly,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            dialer.DialContext,
			ForceAttemptHTTP2:      true,
			TLSHandshakeTimeout:    10 * time.Second,
			ResponseHeaderTimeout:  pictureDownloadTimeout,
			MaxResponseHeaderBytes: 64 << 10,
			IdleConnTimeout:        30 * time.Second,
		},
	}
}

// fetchImageURL downloads an https image of at most maxPictureInputBytes
// whose Content-Type is JPEG, PNG or WebP.
func fetchImageURL(ctx context.Context, hc *http.Client, raw string) ([]byte, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("image_url must be an https:// URL")
	}
	ctx, cancel := context.WithTimeout(ctx, pictureDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("image_url is not usable: %v", err)
	}
	req.Header.Set("Accept", "image/jpeg, image/png, image/webp")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading image_url failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading image_url failed: HTTP %d", resp.StatusCode)
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !pictureContentTypes[ct] {
		got := ct
		if got == "" {
			got = "no Content-Type"
		}
		return nil, fmt.Errorf("image_url returned %s; only JPEG, PNG or WebP images are accepted", got)
	}
	if resp.ContentLength > maxPictureInputBytes {
		return nil, errPictureTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPictureInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("downloading image_url failed: %v", err)
	}
	if len(data) > maxPictureInputBytes {
		return nil, errPictureTooLarge
	}
	return data, nil
}

// pictureInfo describes the input image as it appears upright.
type pictureInfo struct {
	Format string
	Width  int
	Height int
}

// prepareGroupPicture turns a JPEG, PNG or WebP into the 640x640 JPEG
// WhatsApp takes: centre-cropped to a square, scaled (up or down), upright,
// with any transparency flattened onto white.
//
// The orientation is applied after cropping and scaling: every EXIF
// orientation maps the centre square of the stored image onto the centre
// square of the upright one, so the result is the same and only 640x640
// pixels are moved instead of the full photo.
func prepareGroupPicture(data []byte) ([]byte, pictureInfo, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, pictureInfo{}, errors.New("the image is not a JPEG, PNG or WebP file")
	}
	switch format {
	case "jpeg", "png", "webp":
	default:
		return nil, pictureInfo{}, fmt.Errorf("%s images are not supported; use JPEG, PNG or WebP", strings.ToUpper(format))
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, pictureInfo{}, errors.New("the image has no pixels")
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxPicturePixels {
		return nil, pictureInfo{}, fmt.Errorf("the image is %dx%d; the limit is %d megapixels", cfg.Width, cfg.Height, maxPicturePixels/1_000_000)
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, pictureInfo{}, fmt.Errorf("the %s image could not be decoded: %v", strings.ToUpper(format), err)
	}
	orientation := 1
	if format == "jpeg" {
		orientation = jpegOrientation(data)
	}

	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	x0, y0 := b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2
	square := image.Rect(x0, y0, x0+side, y0+side)

	dst := image.NewRGBA(image.Rect(0, 0, groupPictureSide, groupPictureSide))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, square, xdraw.Over, nil)
	out := orientRGBA(dst, orientation)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: groupPictureJPEGQuality}); err != nil {
		return nil, pictureInfo{}, fmt.Errorf("encoding the JPEG failed: %v", err)
	}
	info := pictureInfo{Format: format, Width: b.Dx(), Height: b.Dy()}
	if orientation >= 5 {
		info.Width, info.Height = info.Height, info.Width
	}
	return buf.Bytes(), info, nil
}

// jpegOrientation returns the EXIF orientation (1-8) of a JPEG, or 1 when it
// is absent or unreadable. Go's JPEG decoder ignores EXIF, so without this a
// phone photo taken in portrait would come out sideways.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return 1
		}
		marker := data[i+1]
		switch {
		case marker == 0xFF: // fill byte
			i++
			continue
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7): // no length field
			i += 2
			continue
		case marker == 0xDA || marker == 0xD9: // image data starts; EXIF comes before it
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2:]))
		if segLen < 2 || i+2+segLen > len(data) {
			return 1
		}
		if seg := data[i+4 : i+2+segLen]; marker == 0xE1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return tiffOrientation(seg[6:])
		}
		i += 2 + segLen
	}
	return 1
}

// tiffOrientation reads tag 0x0112 from the first IFD of an EXIF TIFF block.
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	if bo.Uint16(t[2:]) != 42 {
		return 1
	}
	ifd := uint64(bo.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > uint64(len(t)) {
		return 1
	}
	n := int(bo.Uint16(t[ifd:]))
	for k := 0; k < n; k++ {
		e := int(ifd) + 2 + 12*k
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) != 0x0112 {
			continue
		}
		if bo.Uint16(t[e+2:]) != 3 { // SHORT
			return 1
		}
		if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
			return v
		}
		return 1
	}
	return 1
}

// orientRGBA returns img turned upright for an EXIF orientation: 2 mirrors,
// 3 rotates 180°, 4 flips, 5 transposes, 6 rotates 90° clockwise, 7
// transverses, 8 rotates 90° counter-clockwise. 1 or unknown is unchanged.
func orientRGBA(img *image.RGBA, orientation int) *image.RGBA {
	if orientation < 2 || orientation > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch orientation {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			out.SetRGBA(x, y, img.RGBAAt(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return out
}

// loadPictureSource returns the raw image bytes for the request's source.
func loadPictureSource(ctx context.Context, req groupPictureRequest, source string, roots []string, hc *http.Client) ([]byte, error) {
	switch source {
	case "image_url":
		return fetchImageURL(ctx, hc, req.ImageURL)
	case "image_path":
		return readImageFile(req.ImagePath, roots)
	case "image_base64":
		return decodeImageBase64(req.ImageBase64)
	}
	return nil, fmt.Errorf("unknown image source %q", source)
}

// pictureEditBlocked returns why this account may not change the picture, or
// "" when it may or when that cannot be told from the group info (WhatsApp
// then decides). Every member may edit a group's info, picture included,
// unless the group is locked to admins; a community's info is admin-only.
// Self missing from a community's participant list is "unknown": WhatsApp
// lists only some of a community's members there.
func pictureEditBlocked(info *types.GroupInfo, self []types.JID) string {
	if info == nil {
		return ""
	}
	admin, known := adminStatus(info, self).(bool)
	if !known || admin {
		return ""
	}
	if info.IsParent {
		return "only community admins can change the community's picture"
	}
	if info.IsLocked {
		return "only group admins can change this group's picture (the group's info is locked to admins)"
	}
	return ""
}

// pictureSetError maps SetGroupPhoto failures onto a status and a message
// the caller can act on.
func pictureSetError(err error, community bool) (int, string) {
	who := "group admins can change this group's picture"
	if community {
		who = "community admins can change the community's picture"
	}
	switch {
	case errors.Is(err, whatsmeow.ErrInvalidImageFormat):
		return http.StatusBadGateway, "WhatsApp rejected the converted image: " + err.Error()
	case errors.Is(err, whatsmeow.ErrIQNotAuthorized), errors.Is(err, whatsmeow.ErrIQForbidden):
		return http.StatusForbidden, "only " + who
	case errors.Is(err, whatsmeow.ErrIQNotFound):
		return http.StatusNotFound, "that group does not exist"
	case errors.Is(err, whatsmeow.ErrIQRateOverLimit):
		return http.StatusTooManyRequests, "WhatsApp is rate-limiting this account; wait a few minutes and try again"
	}
	return http.StatusBadGateway, "setting the picture failed: " + err.Error()
}

// communityPictureFallback reports whether a picture read should be retried
// as a community query: the plain w:profile:picture query answers 401 for a
// community to an account that is not in the parent group itself.
func communityPictureFallback(jid types.JID, err error) bool {
	return jid.Server == types.GroupServer && errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized)
}

func registerGroupPictureRoute(mux *http.ServeMux, client *whatsmeow.Client, auth func(http.HandlerFunc) http.HandlerFunc, mediaRoots []string, hc *http.Client) {
	// POST /api/group/picture {"group_jid", one of image_url | image_path | image_base64, or "remove": true}
	mux.HandleFunc("/api/group/picture", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			jsonErr(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxPictureRequestBytes)
		var req groupPictureRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				jsonErr(w, http.StatusRequestEntityTooLarge, errPictureTooLarge.Error())
				return
			}
			jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		jid, err := parseGroupJID(req.GroupJID, "group_jid")
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		source, err := req.source()
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		// One budget for the download and every WhatsApp round-trip, under
		// the server's 60s WriteTimeout.
		ctx, cancel := context.WithTimeout(r.Context(), groupRequestTimeout)
		defer cancel()

		// The image is checked and converted before WhatsApp is involved, so
		// bad input is a 400 whatever the connection state.
		var avatar []byte
		var meta pictureInfo
		if source != "remove" {
			raw, err := loadPictureSource(ctx, req, source, mediaRoots, hc)
			if err == nil {
				avatar, meta, err = prepareGroupPicture(raw)
			}
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, errPictureTooLarge) {
					status = http.StatusRequestEntityTooLarge
				}
				jsonErr(w, status, err.Error())
				return
			}
		}
		if !requireClient(w, client) {
			return
		}

		// Read the group first: it says whether this is a community and lets
		// a known non-admin get a clear answer without a write being sent.
		info, ierr := fetchGroupInfo(ctx, client, jid)
		switch {
		case errors.Is(ierr, whatsmeow.ErrGroupNotFound):
			jsonErr(w, http.StatusNotFound, "that group does not exist")
			return
		case errors.Is(ierr, whatsmeow.ErrNotInGroup):
			jsonErr(w, http.StatusForbidden, "you are not a member of that group")
			return
		case ierr != nil:
			info = nil // unknown; let WhatsApp decide
		}
		if msg := pictureEditBlocked(info, selfJIDs(client)); msg != "" {
			jsonErr(w, http.StatusForbidden, msg)
			return
		}
		isCommunity := info != nil && info.IsParent

		pictureID, err := client.SetGroupPhoto(ctx, jid, avatar)
		if err != nil {
			status, msg := pictureSetError(err, isCommunity)
			jsonErr(w, status, msg)
			return
		}
		resp := map[string]any{
			"success":      true,
			"group_jid":    jid.String(),
			"is_community": nil,
			"removed":      avatar == nil,
			"picture_id":   nil,
		}
		if info != nil {
			resp["is_community"] = isCommunity
		}
		if avatar != nil {
			resp["picture_id"] = pictureID
			resp["image"] = map[string]any{
				"source":          source,
				"format":          meta.Format,
				"original_width":  meta.Width,
				"original_height": meta.Height,
				"width":           groupPictureSide,
				"height":          groupPictureSide,
				"bytes":           len(avatar),
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}))
}

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif" // registers GIF in the test binary so its rejection is exercised
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// bandsImage draws the layout the crop tests look for: along the long side,
// green outside the centre square, and inside it red then blue. A correct
// centre crop therefore contains no green: red in its first half, blue in
// its second.
func bandsImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	long, short := max(w, h), min(w, h)
	off := (long - short) / 2
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := x
			if h > w {
				t = y
			}
			c := color.RGBA{0, 200, 0, 255}
			switch {
			case t < off || t >= off+short:
			case t < off+short/2:
				c = color.RGBA{220, 0, 0, 255}
			default:
				c = color.RGBA{0, 0, 220, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// colourName classifies a pixel loosely enough to survive JPEG compression.
func colourName(c color.Color) string {
	r, g, b, _ := c.RGBA()
	r, g, b = r>>8, g>>8, b>>8
	switch {
	case r > 230 && g > 230 && b > 230:
		return "white"
	case r > 150 && g < 90 && b < 90:
		return "red"
	case g > 150 && r < 90 && b < 90:
		return "green"
	case b > 150 && r < 90 && g < 90:
		return "blue"
	}
	return fmt.Sprintf("rgb(%d,%d,%d)", r, g, b)
}

// decodeOutput checks the bridge's output is a 640x640 JPEG and decodes it.
func decodeOutput(t *testing.T, out []byte) image.Image {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || format != "jpeg" || cfg.Width != groupPictureSide || cfg.Height != groupPictureSide {
		t.Fatalf("output = %s %dx%d (err %v), want jpeg %dx%d", format, cfg.Width, cfg.Height, err, groupPictureSide, groupPictureSide)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// wantPixels asserts colour names at output coordinates.
func wantPixels(t *testing.T, img image.Image, want map[image.Point]string) {
	t.Helper()
	for p, name := range want {
		if got := colourName(img.At(p.X, p.Y)); got != name {
			t.Errorf("pixel %v = %s, want %s", p, got, name)
		}
	}
}

var (
	// Wide input: red on the left, blue on the right, nothing green at the edges.
	wideCrop = map[image.Point]string{{4, 320}: "red", {80, 320}: "red", {560, 320}: "blue", {635, 320}: "blue"}
	// Tall input, or wide input rotated upright: red on top, blue below.
	tallCrop = map[image.Point]string{{320, 4}: "red", {320, 80}: "red", {320, 560}: "blue", {320, 635}: "blue"}
)

func TestPrepareGroupPictureFormats(t *testing.T) {
	webp, err := os.ReadFile(filepath.Join("testdata", "bands-64x32.webp"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name         string
		data         []byte
		wantFormat   string
		wantW, wantH int
		pixels       map[image.Point]string
	}{
		{"wide JPEG", encodeJPEG(t, bandsImage(300, 100)), "jpeg", 300, 100, wideCrop},
		{"tall JPEG", encodeJPEG(t, bandsImage(120, 360)), "jpeg", 120, 360, tallCrop},
		{"wide PNG", encodePNG(t, bandsImage(900, 450)), "png", 900, 450, wideCrop},
		{"tall PNG", encodePNG(t, bandsImage(50, 80)), "png", 50, 80, tallCrop},
		{"WebP", webp, "webp", 64, 32, wideCrop},
		{"large photo scales down", encodeJPEG(t, bandsImage(3000, 2000)), "jpeg", 3000, 2000, wideCrop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, info, err := prepareGroupPicture(tc.data)
			if err != nil {
				t.Fatal(err)
			}
			if info.Format != tc.wantFormat || info.Width != tc.wantW || info.Height != tc.wantH {
				t.Errorf("info = %+v, want %s %dx%d", info, tc.wantFormat, tc.wantW, tc.wantH)
			}
			wantPixels(t, decodeOutput(t, out), tc.pixels)
		})
	}
}

func TestPrepareGroupPictureUpscalesAndFlattensTransparency(t *testing.T) {
	small := image.NewNRGBA(image.Rect(0, 0, 10, 10)) // fully transparent
	for x := 5; x < 10; x++ {
		for y := 0; y < 10; y++ {
			small.SetNRGBA(x, y, color.NRGBA{0, 0, 220, 255})
		}
	}
	out, info, err := prepareGroupPicture(encodePNG(t, small))
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 10 || info.Height != 10 {
		t.Errorf("info = %+v", info)
	}
	wantPixels(t, decodeOutput(t, out), map[image.Point]string{{100, 320}: "white", {540, 320}: "blue"})
}

// pngWithSize returns a 1x1 PNG whose header claims w x h, with a valid CRC,
// so only the header (never the pixel data) can be what gets it rejected.
func pngWithSize(t *testing.T, w, h uint32) []byte {
	t.Helper()
	data := encodePNG(t, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	binary.BigEndian.PutUint32(data[16:], w)
	binary.BigEndian.PutUint32(data[20:], h)
	binary.BigEndian.PutUint32(data[29:], crc32.ChecksumIEEE(data[12:29]))
	return data
}

func TestPrepareGroupPictureRejects(t *testing.T) {
	var gifBuf bytes.Buffer
	if err := gif.Encode(&gifBuf, image.NewPaletted(image.Rect(0, 0, 4, 4), []color.Color{color.Black, color.White}), nil); err != nil {
		t.Fatal(err)
	}
	truncated := encodePNG(t, bandsImage(40, 20))
	truncated = truncated[:len(truncated)/2]
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"GIF", gifBuf.Bytes(), "GIF images are not supported"},
		{"not an image", []byte("hello, this is text"), "not a JPEG, PNG or WebP"},
		{"empty", nil, "not a JPEG, PNG or WebP"},
		{"decompression bomb", pngWithSize(t, 20000, 20000), "limit is 50 megapixels"},
		{"truncated PNG", truncated, "could not be decoded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := prepareGroupPicture(tc.data)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// withEXIF inserts an APP0 segment and then an APP1 EXIF segment holding one
// orientation tag right after the JPEG's SOI marker.
func withEXIF(jpegData []byte, orientation uint16, littleEndian bool) []byte {
	var bo binary.AppendByteOrder = binary.BigEndian
	tiff := []byte("MM")
	if littleEndian {
		bo, tiff = binary.LittleEndian, []byte("II")
	}
	tiff = bo.AppendUint16(tiff, 42)
	tiff = bo.AppendUint32(tiff, 8) // IFD0 offset
	tiff = bo.AppendUint16(tiff, 2) // two entries
	tiff = bo.AppendUint16(tiff, 0x010F)
	tiff = bo.AppendUint16(tiff, 2) // ASCII "Make", ignored
	tiff = bo.AppendUint32(tiff, 4)
	tiff = append(tiff, 'F', 'a', 'k', 0)
	tiff = bo.AppendUint16(tiff, 0x0112)
	tiff = bo.AppendUint16(tiff, 3) // SHORT
	tiff = bo.AppendUint32(tiff, 1)
	tiff = bo.AppendUint16(tiff, orientation)
	tiff = bo.AppendUint16(tiff, 0)
	tiff = bo.AppendUint32(tiff, 0) // no next IFD

	segment := func(marker byte, payload []byte) []byte {
		s := []byte{0xFF, marker}
		s = binary.BigEndian.AppendUint16(s, uint16(len(payload)+2))
		return append(s, payload...)
	}
	out := append([]byte{}, jpegData[:2]...)
	out = append(out, segment(0xE0, []byte("JFIF\x00\x01\x02\x00\x00\x01\x00\x01\x00\x00"))...)
	out = append(out, segment(0xE1, append([]byte("Exif\x00\x00"), tiff...))...)
	return append(out, jpegData[2:]...)
}

func TestJPEGOrientation(t *testing.T) {
	plain := encodeJPEG(t, bandsImage(8, 4))
	for o := uint16(1); o <= 8; o++ {
		for _, le := range []bool{true, false} {
			if got := jpegOrientation(withEXIF(plain, o, le)); got != int(o) {
				t.Errorf("orientation %d (little-endian %v) read as %d", o, le, got)
			}
		}
	}
	cases := map[string][]byte{
		"no EXIF":           plain,
		"not a JPEG":        encodePNG(t, bandsImage(8, 4)),
		"out-of-range tag":  withEXIF(plain, 9, true),
		"truncated segment": withEXIF(plain, 6, false)[:30],
		"empty":             nil,
	}
	for name, data := range cases {
		if got := jpegOrientation(data); got != 1 {
			t.Errorf("%s: orientation = %d, want 1", name, got)
		}
	}
}

func TestOrientRGBA(t *testing.T) {
	// 3x2 source; each pixel's red channel is its index (x + 3y).
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			src.SetRGBA(x, y, color.RGBA{uint8(x + 3*y), 0, 0, 255})
		}
	}
	// For each EXIF orientation, the stored pixel that ends up at the upright
	// image's top-left and top-right, per the EXIF definition of each value.
	cases := []struct {
		orientation     int
		w, h            int
		topLeft, topRig image.Point
	}{
		{1, 3, 2, image.Pt(0, 0), image.Pt(2, 0)},
		{2, 3, 2, image.Pt(2, 0), image.Pt(0, 0)},
		{3, 3, 2, image.Pt(2, 1), image.Pt(0, 1)},
		{4, 3, 2, image.Pt(0, 1), image.Pt(2, 1)},
		{5, 2, 3, image.Pt(0, 0), image.Pt(0, 1)},
		{6, 2, 3, image.Pt(0, 1), image.Pt(0, 0)},
		{7, 2, 3, image.Pt(2, 1), image.Pt(2, 0)},
		{8, 2, 3, image.Pt(2, 0), image.Pt(2, 1)},
	}
	idx := func(p image.Point) uint8 { return uint8(p.X + 3*p.Y) }
	for _, tc := range cases {
		out := orientRGBA(src, tc.orientation)
		if out.Bounds().Dx() != tc.w || out.Bounds().Dy() != tc.h {
			t.Errorf("orientation %d: size %v, want %dx%d", tc.orientation, out.Bounds().Size(), tc.w, tc.h)
			continue
		}
		if got := out.RGBAAt(0, 0).R; got != idx(tc.topLeft) {
			t.Errorf("orientation %d: top-left is stored pixel %d, want %d", tc.orientation, got, idx(tc.topLeft))
		}
		if got := out.RGBAAt(tc.w-1, 0).R; got != idx(tc.topRig) {
			t.Errorf("orientation %d: top-right is stored pixel %d, want %d", tc.orientation, got, idx(tc.topRig))
		}
	}
}

// A wide photo stored sideways with orientation 6 (rotate 90° clockwise)
// must come out upright: the stored left (red) half on top.
func TestPrepareGroupPictureAppliesEXIFOrientation(t *testing.T) {
	stored := encodeJPEG(t, bandsImage(400, 200))
	out, info, err := prepareGroupPicture(withEXIF(stored, 6, true))
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 200 || info.Height != 400 {
		t.Errorf("info = %+v, want the upright 200x400", info)
	}
	wantPixels(t, decodeOutput(t, out), tallCrop)

	out, _, err = prepareGroupPicture(withEXIF(stored, 8, false))
	if err != nil {
		t.Fatal(err)
	}
	// Rotated the other way, the stored right (blue) half is on top.
	wantPixels(t, decodeOutput(t, out), map[image.Point]string{{320, 80}: "blue", {320, 560}: "red"})
}

func TestGroupPictureRequestSource(t *testing.T) {
	cases := []struct {
		name    string
		req     groupPictureRequest
		want    string
		wantErr string
	}{
		{"url", groupPictureRequest{ImageURL: "https://example.com/a.png"}, "image_url", ""},
		{"path", groupPictureRequest{ImagePath: "/tmp/a.png"}, "image_path", ""},
		{"base64", groupPictureRequest{ImageBase64: "aGk="}, "image_base64", ""},
		{"remove", groupPictureRequest{Remove: true}, "remove", ""},
		{"blank fields do not count", groupPictureRequest{ImageURL: "  ", ImagePath: "/tmp/a.png"}, "image_path", ""},
		{"nothing", groupPictureRequest{}, "", "give exactly one of"},
		{"two sources", groupPictureRequest{ImageURL: "https://example.com/a.png", ImageBase64: "aGk="}, "", "image_url and image_base64"},
		{"remove with an image", groupPictureRequest{Remove: true, ImagePath: "/tmp/a.png"}, "", "remove=true takes no image; drop image_path"},
	}
	for _, tc := range cases {
		got, err := tc.req.source()
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: source = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestDecodeImageBase64(t *testing.T) {
	raw := []byte{0xFF, 0xD8, 0xFF, 0xFB, 0xEF, 0xBE, 0x00, 0x3E}
	std := base64.StdEncoding.EncodeToString(raw)
	ok := map[string]string{
		"standard":           std,
		"no padding":         base64.RawStdEncoding.EncodeToString(raw),
		"URL-safe":           base64.URLEncoding.EncodeToString(raw),
		"line breaks":        std[:4] + "\n" + std[4:8] + "\r\n " + std[8:],
		"data URL":           "data:image/jpeg;base64," + std,
		"surrounding spaces": "  " + std + "\n",
	}
	for name, in := range ok {
		got, err := decodeImageBase64(in)
		if err != nil || !bytes.Equal(got, raw) {
			t.Errorf("%s: got %x, %v", name, got, err)
		}
	}
	bad := map[string]string{
		"not base64":         "this is not base64!",
		"data URL not b64":   "data:image/png,rawbytes",
		"empty":              "   ",
		"empty data URL":     "data:image/png;base64,",
		"data URL no comma ": "data:image/png;base64",
	}
	for name, in := range bad {
		if _, err := decodeImageBase64(in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	huge := strings.Repeat("A", maxPictureInputBytes/3*4+8)
	if _, err := decodeImageBase64(huge); !errors.Is(err, errPictureTooLarge) {
		t.Errorf("oversized base64: err = %v, want errPictureTooLarge", err)
	}
}

func TestReadImageFile(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved // macOS: /var -> /private/var
	}
	inside := filepath.Join(root, "pic.png")
	if err := os.WriteFile(inside, []byte("png bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "pic.png")
	if err := os.WriteFile(outside, []byte("png bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(root, "big.png")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxPictureInputBytes + 1); err != nil { // sparse; nothing is written
		t.Fatal(err)
	}
	_ = f.Close()
	roots := []string{root}

	if got, err := readImageFile(inside, roots); err != nil || string(got) != "png bytes" {
		t.Errorf("inside root: %q, %v", got, err)
	}
	for name, tc := range map[string]struct{ path, want string }{
		"outside the roots": {outside, "image_path"},
		"relative path":     {"pic.png", "must be absolute"},
		"directory":         {root, "not a regular file"},
		"missing":           {filepath.Join(root, "nope.png"), "image_path"},
	} {
		_, err := readImageFile(tc.path, roots)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "media_path") {
			t.Errorf("%s: err = %v, want it to mention %q (and not media_path)", name, err, tc.want)
		}
	}
	if _, err := readImageFile(big, roots); !errors.Is(err, errPictureTooLarge) {
		t.Errorf("big file: err = %v, want errPictureTooLarge", err)
	}
}

func TestFetchImageURL(t *testing.T) {
	pngData := encodePNG(t, bandsImage(20, 10))
	big := bytes.Repeat([]byte{0}, maxPictureInputBytes+1)
	mux := http.NewServeMux()
	mux.HandleFunc("/ok.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngData)
	})
	mux.HandleFunc("/ok-with-params", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "Image/WebP; charset=binary")
		_, _ = w.Write([]byte("webp bytes"))
	})
	mux.HandleFunc("/page.html", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html></html>"))
	})
	mux.HandleFunc("/untyped", func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil // stop net/http sniffing one
		_, _ = w.Write(pngData)
	})
	mux.HandleFunc("/gif", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write([]byte("GIF89a"))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	mux.HandleFunc("/big-declared", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", fmt.Sprint(len(big)))
		_, _ = w.Write(big)
	})
	mux.HandleFunc("/big-streamed", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		for i := 0; i < len(big); i += 1 << 20 {
			_, _ = w.Write(big[i:min(i+1<<20, len(big))])
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/to-http", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/pic.png", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	hc := srv.Client() // trusts the test certificate; loopback is allowed here
	hc.CheckRedirect = httpsRedirectsOnly
	ctx := context.Background()

	if got, err := fetchImageURL(ctx, hc, srv.URL+"/ok.png"); err != nil || !bytes.Equal(got, pngData) {
		t.Fatalf("ok.png: %d bytes, %v", len(got), err)
	}
	if got, err := fetchImageURL(ctx, hc, srv.URL+"/ok-with-params"); err != nil || string(got) != "webp bytes" {
		t.Fatalf("content type with parameters: %q, %v", got, err)
	}
	cases := []struct{ path, want string }{
		{"/page.html", "returned text/html; only JPEG, PNG or WebP"},
		{"/untyped", "returned no Content-Type"},
		{"/gif", "returned image/gif"},
		{"/missing", "HTTP 404"},
		{"/to-http", "non-https"},
		{"/loop", "redirected more than 3 times"},
	}
	for _, tc := range cases {
		_, err := fetchImageURL(ctx, hc, srv.URL+tc.path)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.path, err, tc.want)
		}
	}
	for _, path := range []string{"/big-declared", "/big-streamed"} {
		if _, err := fetchImageURL(ctx, hc, srv.URL+path); !errors.Is(err, errPictureTooLarge) {
			t.Errorf("%s: err = %v, want errPictureTooLarge", path, err)
		}
	}
	for _, raw := range []string{"http://example.com/a.png", "ftp://example.com/a.png", "https://", "example.com/a.png", "file:///etc/hosts"} {
		if _, err := fetchImageURL(ctx, hc, raw); err == nil || !strings.Contains(err.Error(), "must be an https:// URL") {
			t.Errorf("%q: err = %v", raw, err)
		}
	}
	// The production client refuses the loopback test server at dial time.
	_, err := fetchImageURL(ctx, newPictureHTTPClient(), srv.URL+"/ok.png")
	if err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Errorf("production client against loopback: err = %v", err)
	}
}

func TestIsPublicAddr(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.8.9.10", "::1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.10",
		"169.254.169.254", "100.64.0.1", "100.100.100.100", "0.0.0.0", "0.1.2.3", "::", "fe80::1",
		"fc00::1", "fd12:3456::1", "::ffff:127.0.0.1", "::ffff:192.168.0.1", "224.0.0.1", "ff02::1",
		"255.255.255.255", "240.0.0.1", "198.18.0.1", "192.0.0.8",
	}
	for _, s := range blocked {
		if isPublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be refused", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.215.14", "2606:4700:4700::1111", "::ffff:8.8.8.8", "172.32.0.1", "100.128.0.1"} {
		if !isPublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
	if isPublicAddr(netip.Addr{}) {
		t.Error("the zero address should be refused")
	}
	if err := publicAddrOnly("tcp4", "127.0.0.1:443", nil); err == nil {
		t.Error("publicAddrOnly let loopback through")
	}
	if err := publicAddrOnly("tcp4", "8.8.8.8:443", nil); err != nil {
		t.Errorf("publicAddrOnly refused a public address: %v", err)
	}
}

func TestPictureEditBlocked(t *testing.T) {
	selfPN := types.NewJID("10000000001", types.DefaultUserServer)
	selfLID := types.NewJID("200000000000001", types.HiddenUserServer)
	other := types.NewJID("10000000002", types.DefaultUserServer)
	self := []types.JID{selfPN, selfLID}
	member := func(jid types.JID, admin bool) types.GroupParticipant {
		return types.GroupParticipant{JID: jid, IsAdmin: admin}
	}
	info := func(locked, parent bool, parts ...types.GroupParticipant) *types.GroupInfo {
		return &types.GroupInfo{
			GroupLocked:  types.GroupLocked{IsLocked: locked},
			GroupParent:  types.GroupParent{IsParent: parent},
			Participants: parts,
		}
	}
	cases := []struct {
		name string
		info *types.GroupInfo
		want string
	}{
		{"no info", nil, ""},
		{"admin of a locked group", info(true, false, member(selfPN, true)), ""},
		{"admin listed by LID", info(true, false, member(selfLID, true)), ""},
		{"member of an unlocked group may edit", info(false, false, member(selfPN, false), member(other, true)), ""},
		{"member of a locked group", info(true, false, member(selfPN, false), member(other, true)), "only group admins"},
		{"community admin", info(false, true, member(selfLID, true)), ""},
		{"community member listed as non-admin", info(false, true, member(selfPN, false)), "only community admins"},
		{"self not listed in a community: unknown", info(false, true, member(other, true)), ""},
		{"self not listed in a locked group: unknown", info(true, false, member(other, true)), ""},
	}
	for _, tc := range cases {
		got := pictureEditBlocked(tc.info, self)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPictureSetError(t *testing.T) {
	cases := []struct {
		err        error
		community  bool
		wantStatus int
		wantMsg    string
	}{
		{fmt.Errorf("wrapped: %w", whatsmeow.ErrIQNotAuthorized), false, http.StatusForbidden, "only group admins can change this group's picture"},
		{whatsmeow.ErrIQForbidden, true, http.StatusForbidden, "only community admins can change the community's picture"},
		{fmt.Errorf("%w: %w", whatsmeow.ErrInvalidImageFormat, whatsmeow.ErrIQNotAcceptable), false, http.StatusBadGateway, "rejected the converted image"},
		{whatsmeow.ErrIQNotFound, false, http.StatusNotFound, "does not exist"},
		{whatsmeow.ErrIQRateOverLimit, true, http.StatusTooManyRequests, "rate-limiting"},
		{errors.New("socket closed"), false, http.StatusBadGateway, "setting the picture failed: socket closed"},
	}
	for _, tc := range cases {
		status, msg := pictureSetError(tc.err, tc.community)
		if status != tc.wantStatus || !strings.Contains(msg, tc.wantMsg) {
			t.Errorf("pictureSetError(%v, %v) = %d %q, want %d containing %q", tc.err, tc.community, status, msg, tc.wantStatus, tc.wantMsg)
		}
	}
}

func TestCommunityPictureFallback(t *testing.T) {
	group := types.NewJID("120363000000000100", types.GroupServer)
	user := types.NewJID("10000000001", types.DefaultUserServer)
	unauthorized := fmt.Errorf("%w: %w", whatsmeow.ErrProfilePictureUnauthorized, whatsmeow.ErrIQNotAuthorized)
	cases := []struct {
		jid  types.JID
		err  error
		want bool
	}{
		{group, unauthorized, true},
		{user, unauthorized, false}, // a contact hiding their picture is final
		{group, nil, false},
		{group, whatsmeow.ErrProfilePictureNotSet, false},
		{group, errors.New("timeout"), false},
	}
	for _, tc := range cases {
		if got := communityPictureFallback(tc.jid, tc.err); got != tc.want {
			t.Errorf("communityPictureFallback(%v, %v) = %v, want %v", tc.jid, tc.err, got, tc.want)
		}
	}
}

// Same approach as TestGroupEndpointValidation: identity auth, nil client.
// Every input problem, the image itself included, must be a 4xx before the
// client is touched; valid input then reads as 503.
func TestGroupPictureEndpointValidation(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	picPath := filepath.Join(root, "pic.png")
	if err := os.WriteFile(picPath, encodePNG(t, bandsImage(40, 20)), 0o600); err != nil {
		t.Fatal(err)
	}
	textPath := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(textPath, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := func(h http.HandlerFunc) http.HandlerFunc { return h }
	mux := http.NewServeMux()
	registerGroupRoutes(mux, nil, identity, []string{root})

	const g = `"group_jid":"120363000000000100@g.us"`
	pngB64 := base64.StdEncoding.EncodeToString(encodePNG(t, bandsImage(40, 20)))
	cases := []struct {
		name       string
		method     string
		body       string
		wantStatus int
	}{
		{"rejects GET", http.MethodGet, "", http.StatusMethodNotAllowed},
		{"rejects invalid JSON", http.MethodPost, "{", http.StatusBadRequest},
		{"requires group_jid", http.MethodPost, `{"remove":true}`, http.StatusBadRequest},
		{"rejects user JID", http.MethodPost, `{"group_jid":"10000000001@s.whatsapp.net","remove":true}`, http.StatusBadRequest},
		{"requires a source", http.MethodPost, `{` + g + `}`, http.StatusBadRequest},
		{"rejects two sources", http.MethodPost, `{` + g + `,"image_url":"https://example.com/a.png","image_path":"` + picPath + `"}`, http.StatusBadRequest},
		{"rejects remove with a source", http.MethodPost, `{` + g + `,"remove":true,"image_base64":"` + pngB64 + `"}`, http.StatusBadRequest},
		{"rejects http URL", http.MethodPost, `{` + g + `,"image_url":"http://example.com/a.png"}`, http.StatusBadRequest},
		{"rejects bad base64", http.MethodPost, `{` + g + `,"image_base64":"%%%"}`, http.StatusBadRequest},
		{"rejects base64 that is not an image", http.MethodPost, `{` + g + `,"image_base64":"aGVsbG8gd29ybGQ="}`, http.StatusBadRequest},
		{"rejects path outside media roots", http.MethodPost, `{` + g + `,"image_path":"/etc/hosts"}`, http.StatusBadRequest},
		{"rejects a file that is not an image", http.MethodPost, `{` + g + `,"image_path":"` + textPath + `"}`, http.StatusBadRequest},
		{"rejects oversized base64", http.MethodPost, `{` + g + `,"image_base64":"` + strings.Repeat("A", maxPictureInputBytes/3*4+8) + `"}`, http.StatusRequestEntityTooLarge},
		{"rejects an oversized body", http.MethodPost, `{` + g + `,"image_base64":"` + strings.Repeat("A", maxPictureRequestBytes) + `"}`, http.StatusRequestEntityTooLarge},
		{"base64 image with no client is 503", http.MethodPost, `{` + g + `,"image_base64":"` + pngB64 + `"}`, http.StatusServiceUnavailable},
		{"path image with no client is 503", http.MethodPost, `{` + g + `,"image_path":"` + picPath + `"}`, http.StatusServiceUnavailable},
		{"community remove with no client is 503", http.MethodPost, `{"group_jid":"120363000000000200@g.us","remove":true}`, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/group/picture", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %.300s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			if !strings.Contains(rec.Body.String(), `"success":false`) {
				t.Fatalf("body should be a JSON error envelope, got %.300s", rec.Body.String())
			}
		})
	}
}

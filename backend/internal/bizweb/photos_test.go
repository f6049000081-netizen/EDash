package bizweb_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
)

type memStore struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (m *memStore) Put(_ context.Context, k string, d []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[k] = d
	return nil
}

func (m *memStore) Get(_ context.Context, k string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.files[k]; ok {
		return d, nil
	}
	return nil, errors.New("no such file")
}

func (m *memStore) Delete(_ context.Context, k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, k)
	return nil
}

// upload posts a file the way a browser's multipart form does.
func (b *browser) upload(path, csrf, name string, data []byte) (int, string) {
	b.e.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("csrf", csrf)
	fw, _ := mw.CreateFormFile("photo", name)
	_, _ = fw.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, b.e.srv.URL+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode, res.Header.Get("Location")
}

func TestOwnersAddProductPhotos(t *testing.T) {
	e := setup(t)
	owner, cashier, merchant, injera := e.shop()
	store := &memStore{files: map[string][]byte{}}
	e.shops.Photos = store
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for i := range img.Pix {
		img.Pix[i] = uint8(i * 7)
	}
	img.Set(0, 0, color.White)
	var pic bytes.Buffer
	_ = png.Encode(&pic, img)

	b := e.browser()
	b.signIn(owner, merchant)
	page := "/business/products/" + injera.String()
	st, html, _ := b.get(page)
	if st != 200 || !strings.Contains(html, `enctype="multipart/form-data"`) || !strings.Contains(html, "No photo yet") {
		t.Fatalf("product page: %d", st)
	}
	csrf := csrfRE.FindStringSubmatch(html)[1]

	if _, loc := b.upload(page+"/photo", csrf, "menu.pdf", []byte("%PDF-1.7 not a photo")); loc != page+"?msg=photo_bad" {
		t.Fatalf("pdf: %s", loc)
	}
	if st, _ := b.upload(page+"/photo", "wrong", "injera.png", pic.Bytes()); st != http.StatusForbidden {
		t.Fatalf("without the CSRF token: %d", st)
	}
	if _, loc := b.upload(page+"/photo", csrf, "injera.png", pic.Bytes()); loc != page+"?msg=photo_saved" {
		t.Fatalf("upload: %s", loc)
	}
	_, html, _ = b.get(page + "?msg=photo_saved")
	m := regexp.MustCompile(`<img class="photo" src="([^"]+)"`).FindStringSubmatch(html)
	if m == nil || !strings.Contains(html, "Photo saved.") {
		t.Fatalf("photo not shown:\n%s", html)
	}
	st, body, loc := b.get(m[1])
	if st != 200 || !strings.HasPrefix(body, "\xff\xd8") {
		t.Fatalf("photo file: %d", st)
	}
	_, list, _ := b.get("/business/products")
	if !strings.Contains(list, `class="thumb" src="`+m[1]+`?size=thumb"`) {
		t.Fatalf("products list lacks the thumbnail:\n%s", list)
	}

	// Another shop's browser can't read it through the console; a cashier can't change it.
	o2, _, m2, _ := e.shop()
	other := e.browser()
	other.signIn(o2, m2)
	if st, _, _ := other.get(m[1]); st != http.StatusNotFound {
		t.Fatalf("another shop read the photo: %d", st)
	}
	c := e.browser()
	c.signIn(cashier, merchant)
	ccsrf := c.csrf("/business/products")
	if _, loc := c.upload(page+"/photo", ccsrf, "x.png", pic.Bytes()); loc != page+"?msg=forbidden" {
		t.Fatalf("cashier upload: %s", loc)
	}

	// Removing it.
	st, body, loc = b.post(page+"/photo", map[string][]string{"csrf": {csrf}, "remove": {"1"}})
	if st != http.StatusSeeOther || loc != page+"?msg=photo_removed" || len(store.files) != 0 {
		t.Fatalf("remove: %d %s %q, %d files left", st, loc, body, len(store.files))
	}
}

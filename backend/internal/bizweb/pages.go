package bizweb

import (
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"superapp/backend/internal/commerce"
	"superapp/backend/internal/merchants"
	"superapp/backend/internal/money"
	"superapp/backend/internal/payouts"
	"superapp/backend/internal/photos"
)

func tokensEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// ---------------------------------------------------------------------------
// Home
// ---------------------------------------------------------------------------

type homeData struct {
	Summary  *merchants.Summary
	Shop     *commerce.Shop
	Orders   []commerce.Order
	Waiting  int
	LowStock []commerce.Product
}

func (s *Server) home(w http.ResponseWriter, r *http.Request, sess *Session) {
	ctx := r.Context()
	d := homeData{}
	var err error
	if d.Summary, err = s.Merchants.Summary(ctx, sess.UserID, sess.MerchantID); err != nil {
		s.serverError(w, r, sess, err)
		return
	}
	if d.Shop, err = s.Shops.ShopSettings(ctx, sess.UserID, sess.MerchantID); err != nil {
		s.serverError(w, r, sess, err)
		return
	}
	if d.Orders, err = s.Shops.ShopOrders(ctx, sess.UserID, sess.MerchantID, true, 100); err != nil {
		s.serverError(w, r, sess, err)
		return
	}
	for _, o := range d.Orders {
		if o.Status == "placed" {
			d.Waiting++
		}
	}
	for _, p := range d.Shop.Products {
		if p.Stock != nil && *p.Stock <= 5 && p.Status == "active" {
			d.LowStock = append(d.LowStock, p)
		}
	}
	if len(d.Orders) > 8 {
		d.Orders = d.Orders[:8]
	}
	s.renderPage(w, r, http.StatusOK, "home", sess, page{Data: d, Refresh: 20})
}

// ---------------------------------------------------------------------------
// Orders
// ---------------------------------------------------------------------------

func (s *Server) orders(w http.ResponseWriter, r *http.Request, sess *Session) {
	live := r.URL.Query().Get("state") != "done"
	list, err := s.Shops.ShopOrders(r.Context(), sess.UserID, sess.MerchantID, live, 100)
	if err != nil {
		s.serverError(w, r, sess, err)
		return
	}
	p := page{Data: map[string]any{"Live": live, "Orders": list}}
	if live {
		p.Refresh = 20
	}
	s.renderPage(w, r, http.StatusOK, "orders", sess, p)
}

func (s *Server) order(w http.ResponseWriter, r *http.Request, sess *Session) {
	id, ok := parseID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	o, err := s.Shops.ShopOrder(r.Context(), sess.UserID, sess.MerchantID, id)
	if errors.Is(err, commerce.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, sess, err)
		return
	}
	s.renderPage(w, r, http.StatusOK, "order", sess, page{Nav: "orders", Data: o})
}

var fourDigits = regexp.MustCompile(`^[0-9]{4}$`)

func (s *Server) orderAction(w http.ResponseWriter, r *http.Request, sess *Session) {
	id, ok := parseID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	action := r.PathValue("action")
	back := "/business/orders/" + id.String()
	switch r.PostFormValue("return") {
	case "list":
		back = "/business/orders"
	case "home":
		back = "/business/"
	}
	var err error
	done := ""
	switch action {
	case "accept", "ready", "on_the_way":
		_, err = s.Shops.Advance(ctx, sess.UserID, sess.MerchantID, id, action)
		done = map[string]string{"accept": "accepted", "ready": "ready", "on_the_way": "on_the_way"}[action]
	case "hand_over":
		code := strings.TrimSpace(r.PostFormValue("code"))
		if !fourDigits.MatchString(code) {
			http.Redirect(w, r, "/business/orders/"+id.String()+"?msg=code_format", http.StatusSeeOther)
			return
		}
		_, err = s.Shops.HandOver(ctx, sess.UserID, sess.MerchantID, id, code)
		done = "handed_over"
	case "cancel":
		note := strings.TrimSpace(r.PostFormValue("note"))
		if note == "" {
			http.Redirect(w, r, "/business/orders/"+id.String()+"?msg=note", http.StatusSeeOther)
			return
		}
		_, err = s.Shops.CancelByShop(ctx, sess.UserID, sess.MerchantID, id, note)
		done = "cancelled"
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		code := s.code(err)
		if code == "" {
			s.serverError(w, r, sess, err)
			return
		}
		if code == "invalid" {
			code = "note"
		}
		http.Redirect(w, r, "/business/orders/"+id.String()+"?msg="+code, http.StatusSeeOther)
		return
	}
	if done == "handed_over" || done == "cancelled" {
		back = "/business/orders"
	}
	http.Redirect(w, r, back+"?msg="+done, http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Products
// ---------------------------------------------------------------------------

func (s *Server) products(w http.ResponseWriter, r *http.Request, sess *Session) {
	shop, err := s.Shops.ShopSettings(r.Context(), sess.UserID, sess.MerchantID)
	if err != nil {
		s.serverError(w, r, sess, err)
		return
	}
	s.renderPage(w, r, http.StatusOK, "products", sess, page{Data: shop})
}

type productData struct {
	ID          string
	Title       string
	Price       string
	Category    string
	Description string
	Stock       string
	Hidden      bool
	Photo       string // the current photo's ID
}

func (s *Server) productForm(w http.ResponseWriter, r *http.Request, sess *Session) {
	d := productData{}
	if r.PathValue("id") != "" {
		id, ok := parseID(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		shop, err := s.Shops.ShopSettings(r.Context(), sess.UserID, sess.MerchantID)
		if err != nil {
			s.serverError(w, r, sess, err)
			return
		}
		var p *commerce.Product
		for i := range shop.Products {
			if shop.Products[i].ID == id {
				p = &shop.Products[i]
			}
		}
		if p == nil {
			http.NotFound(w, r)
			return
		}
		d = productData{ID: p.ID.String(), Title: p.Title, Price: p.Price.Decimal(), Category: p.Category,
			Description: p.Description, Hidden: p.Status == "hidden", Photo: p.Photo}
		if p.Stock != nil {
			d.Stock = strconv.Itoa(*p.Stock)
		}
	}
	s.renderPage(w, r, http.StatusOK, "product", sess, page{Nav: "products", Data: d})
}

func (s *Server) productSave(w http.ResponseWriter, r *http.Request, sess *Session) {
	l := lang(r, sess)
	d := productData{
		ID: r.PathValue("id"), Title: r.PostFormValue("title"), Price: strings.TrimSpace(r.PostFormValue("price")),
		Category: r.PostFormValue("category"), Description: r.PostFormValue("description"),
		Stock: strings.TrimSpace(r.PostFormValue("stock")), Hidden: r.PostFormValue("hidden") != "",
	}
	again := func(msg string) {
		s.renderPage(w, r, http.StatusUnprocessableEntity, "product", sess, page{Nav: "products", Data: d, Error: msg})
	}
	if !sess.Manages() {
		again(translate(l, "m_forbidden"))
		return
	}
	price, err := money.ParseDecimal(strings.ReplaceAll(d.Price, ",", ""), money.ETB)
	if err != nil {
		again(translate(l, "m_price"))
		return
	}
	in := commerce.ProductInput{Title: d.Title, Description: d.Description, Category: d.Category, Price: price, Status: "active"}
	if d.Hidden {
		in.Status = "hidden"
	}
	if r.PostFormValue("archive") != "" {
		in.Status = "archived"
	}
	if d.Stock != "" {
		n, err := strconv.Atoi(d.Stock)
		if err != nil {
			again(translate(l, "m_stock"))
			return
		}
		in.Stock = &n
	}
	ctx := r.Context()
	if d.ID == "" {
		_, err = s.Shops.CreateProduct(ctx, sess.UserID, sess.MerchantID, in)
	} else {
		id, perr := uuid.Parse(d.ID)
		if perr != nil {
			http.NotFound(w, r)
			return
		}
		_, err = s.Shops.UpdateProduct(ctx, sess.UserID, sess.MerchantID, id, in)
	}
	var v *commerce.ValidationError
	var c *commerce.ConflictError
	switch {
	case err == nil:
		http.Redirect(w, r, "/business/products?msg=saved", http.StatusSeeOther)
	case errors.As(err, &v):
		again(v.Msg)
	case errors.As(err, &c):
		again(c.Msg)
	case errors.Is(err, commerce.ErrForbidden):
		again(translate(l, "m_forbidden"))
	case errors.Is(err, commerce.ErrNotFound):
		http.NotFound(w, r)
	default:
		s.serverError(w, r, sess, err)
	}
}

// productPhoto adds, replaces or removes a product's photo (a multipart form with the file in "photo").
func (s *Server) productPhoto(w http.ResponseWriter, r *http.Request, sess *Session) {
	id, ok := parseID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := func(code string) {
		http.Redirect(w, r, "/business/products/"+id.String()+"?msg="+code, http.StatusSeeOther)
	}
	var err error
	if r.PostFormValue("remove") != "" {
		_, err = s.Shops.RemoveProductPhoto(r.Context(), sess.UserID, sess.MerchantID, id)
		if err == nil {
			back("photo_removed")
			return
		}
	} else {
		f, _, ferr := r.FormFile("photo")
		if ferr != nil {
			back("photo_bad")
			return
		}
		defer f.Close()
		data, rerr := io.ReadAll(io.LimitReader(f, photos.MaxSize+1))
		if rerr != nil {
			back("photo_bad")
			return
		}
		_, err = s.Shops.SetProductPhoto(r.Context(), sess.UserID, sess.MerchantID, id, data)
		if err == nil {
			back("photo_saved")
			return
		}
	}
	var v *commerce.ValidationError
	switch {
	case errors.As(err, &v):
		back("photo_bad")
	case errors.Is(err, commerce.ErrPhotosOff):
		back("photos_off")
	case errors.Is(err, commerce.ErrForbidden):
		back("forbidden")
	case errors.Is(err, commerce.ErrNotFound):
		http.NotFound(w, r)
	default:
		s.serverError(w, r, sess, err)
	}
}

// productPhotoFile shows a product photo on the console's own origin, as its CSP allows.
func (s *Server) productPhotoFile(w http.ResponseWriter, r *http.Request, sess *Session) {
	id, ok := parseID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.Shops.ShopProduct(r.Context(), sess.MerchantID, id); err != nil {
		http.NotFound(w, r) // another shop's product
		return
	}
	data, err := s.Shops.ProductPhoto(r.Context(), id, r.PathValue("photo"), r.URL.Query().Get("size") == "thumb")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------------------
// Storefront
// ---------------------------------------------------------------------------

type storefrontData struct {
	Shop                   *commerce.Shop
	Open, Pickup, Delivery bool
	Fee, Area, About       string
}

func (s *Server) storefront(w http.ResponseWriter, r *http.Request, sess *Session) {
	shop, err := s.Shops.ShopSettings(r.Context(), sess.UserID, sess.MerchantID)
	if err != nil {
		s.serverError(w, r, sess, err)
		return
	}
	d := storefrontData{Shop: shop, Open: shop.Open, Pickup: shop.Pickup, Delivery: shop.Delivery,
		Fee: shop.DeliveryFee.Decimal(), Area: shop.DeliveryArea, About: shop.About}
	s.renderPage(w, r, http.StatusOK, "storefront", sess, page{Data: d})
}

func (s *Server) storefrontSave(w http.ResponseWriter, r *http.Request, sess *Session) {
	ctx := r.Context()
	l := lang(r, sess)
	shop, err := s.Shops.ShopSettings(ctx, sess.UserID, sess.MerchantID)
	if err != nil {
		s.serverError(w, r, sess, err)
		return
	}
	d := storefrontData{Shop: shop, Open: r.PostFormValue("open") != "", Pickup: r.PostFormValue("pickup") != "",
		Delivery: r.PostFormValue("delivery") != "", Fee: strings.TrimSpace(r.PostFormValue("fee")),
		Area: r.PostFormValue("area"), About: r.PostFormValue("about")}
	again := func(msg string) {
		s.renderPage(w, r, http.StatusUnprocessableEntity, "storefront", sess, page{Data: d, Error: msg})
	}
	feeText := d.Fee
	if feeText == "" {
		feeText = "0"
	}
	fee, err := money.ParseDecimal(strings.ReplaceAll(feeText, ",", ""), money.ETB)
	if err != nil {
		again(translate(l, "m_fee"))
		return
	}
	_, err = s.Shops.UpdateShop(ctx, sess.UserID, sess.MerchantID, commerce.ShopUpdate{Open: d.Open, Pickup: d.Pickup,
		Delivery: d.Delivery, DeliveryFee: fee, DeliveryArea: d.Area, About: d.About})
	s.afterShopSave(w, r, sess, err, "/business/storefront", again)
}

// storefrontOpen is the switch on the home page: only "taking orders" changes.
func (s *Server) storefrontOpen(w http.ResponseWriter, r *http.Request, sess *Session) {
	ctx := r.Context()
	shop, err := s.Shops.ShopSettings(ctx, sess.UserID, sess.MerchantID)
	if err != nil {
		s.serverError(w, r, sess, err)
		return
	}
	_, err = s.Shops.UpdateShop(ctx, sess.UserID, sess.MerchantID, commerce.ShopUpdate{Open: r.PostFormValue("open") == "1",
		Pickup: shop.Pickup, Delivery: shop.Delivery, DeliveryFee: shop.DeliveryFee, DeliveryArea: shop.DeliveryArea, About: shop.About})
	s.afterShopSave(w, r, sess, err, "/business/", func(msg string) {
		http.Redirect(w, r, "/business/?msg=conflict", http.StatusSeeOther)
	})
}

func (s *Server) afterShopSave(w http.ResponseWriter, r *http.Request, sess *Session, err error, back string, again func(string)) {
	var v *commerce.ValidationError
	var c *commerce.ConflictError
	switch {
	case err == nil:
		http.Redirect(w, r, back+"?msg=saved", http.StatusSeeOther)
	case errors.As(err, &v):
		again(v.Msg)
	case errors.As(err, &c):
		again(c.Msg)
	case errors.Is(err, commerce.ErrForbidden):
		again(translate(lang(r, sess), "m_forbidden"))
	default:
		s.serverError(w, r, sess, err)
	}
}

// ---------------------------------------------------------------------------
// Payments and payouts
// ---------------------------------------------------------------------------

func (s *Server) money(w http.ResponseWriter, r *http.Request, sess *Session) {
	ctx := r.Context()
	pays, err := s.Merchants.Payments(ctx, sess.UserID, sess.MerchantID, nil, uuid.Nil, 50)
	if err != nil {
		s.serverError(w, r, sess, err)
		return
	}
	d := map[string]any{"Payments": pays}
	if sess.SeesMoney() {
		list, err := s.Payouts.List(ctx, sess.UserID, sess.MerchantID, nil, uuid.Nil, 30)
		if err != nil && !errors.Is(err, payouts.ErrForbidden) {
			s.serverError(w, r, sess, err)
			return
		}
		d["Payouts"] = list
		sum, err := s.Merchants.Summary(ctx, sess.UserID, sess.MerchantID)
		if err != nil {
			s.serverError(w, r, sess, err)
			return
		}
		d["Summary"] = sum
	}
	s.renderPage(w, r, http.StatusOK, "money", sess, page{Data: d})
}

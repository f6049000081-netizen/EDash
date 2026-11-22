package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"superapp/backend/internal/commerce"
	"superapp/backend/internal/money"
	"superapp/backend/internal/platform"
)

func (s *Server) commerceRoutes(mux *http.ServeMux, authed, mutating func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /v1/shops", authed(s.listShops))
	mux.Handle("GET /v1/shops/{id}", authed(s.getShop))
	mux.Handle("POST /v1/orders", mutating(s.placeOrder))
	mux.Handle("GET /v1/orders", authed(s.listOrders))
	mux.Handle("GET /v1/orders/{id}", authed(s.getOrder))
	mux.Handle("POST /v1/orders/{id}/cancel", mutating(s.cancelOrder))
	mux.Handle("POST /v1/orders/{id}/received", mutating(s.orderReceived))

	mux.Handle("GET /v1/merchants/{id}/shop", authed(s.getShopSettings))
	mux.Handle("PUT /v1/merchants/{id}/shop", mutating(s.putShopSettings))
	mux.Handle("POST /v1/merchants/{id}/products", mutating(s.createProduct))
	mux.Handle("PUT /v1/merchants/{id}/products/{product}", mutating(s.putProduct))
	mux.Handle("GET /v1/merchants/{id}/orders", authed(s.listShopOrders))
	mux.Handle("POST /v1/merchants/{id}/orders/{order}/{action}", mutating(s.shopOrderAction))
}

// ---------------------------------------------------------------------------
// Customers
// ---------------------------------------------------------------------------

func (s *Server) listShops(w http.ResponseWriter, r *http.Request) {
	list, err := s.Commerce.Shops(r.Context(), 100)
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	out := make([]map[string]any, len(list))
	for i := range list {
		out[i] = shopJSON(&list[i], false)
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) getShop(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	sh, err := s.Commerce.Shop(r.Context(), id)
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, shopJSON(sh, true))
}

func shopJSON(sh *commerce.Shop, withProducts bool) map[string]any {
	m := map[string]any{
		"merchant_id": sh.MerchantID, "name": sh.Name, "category": sh.Category, "city": sh.City, "open": sh.Open,
		"pickup": sh.Pickup, "delivery": sh.Delivery, "delivery_fee": sh.DeliveryFee, "delivery_area": sh.DeliveryArea,
		"about": sh.About, "product_count": sh.ProductCount,
	}
	if withProducts {
		ps := make([]map[string]any, len(sh.Products))
		for i, p := range sh.Products {
			ps[i] = productJSON(p)
		}
		m["products"] = ps
	}
	return m
}

func productJSON(p commerce.Product) map[string]any {
	return map[string]any{
		"id": p.ID, "title": p.Title, "description": p.Description, "category": p.Category, "price": p.Price,
		"stock": p.Stock, "sold_out": p.SoldOut(), "status": p.Status, "updated_at": p.UpdatedAt,
	}
}

func (s *Server) placeOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MerchantID      uuid.UUID `json:"merchant_id"`
		Channel         string    `json:"channel"`
		DeliveryAddress string    `json:"delivery_address"`
		Note            string    `json:"note"`
		Items           []struct {
			ProductID uuid.UUID `json:"product_id"`
			Quantity  int       `json:"quantity"`
		} `json:"items"`
	}
	if !decode(w, r, &body) {
		return
	}
	req := commerce.PlaceRequest{MerchantID: body.MerchantID, Channel: body.Channel, DeliveryAddress: body.DeliveryAddress, Note: body.Note}
	for _, it := range body.Items {
		req.Items = append(req.Items, commerce.LineRequest{ProductID: it.ProductID, Quantity: it.Quantity})
	}
	o, err := s.Commerce.Place(r.Context(), platform.UserFrom(r.Context()), req)
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, orderJSON(o))
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	list, err := s.Commerce.Orders(r.Context(), platform.UserFrom(r.Context()), 50)
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	writeOrders(w, list)
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	o, err := s.Commerce.Order(r.Context(), platform.UserFrom(r.Context()), id)
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, orderJSON(o))
}

func (s *Server) cancelOrder(w http.ResponseWriter, r *http.Request) {
	s.customerOrderStep(w, r, s.Commerce.Cancel)
}

func (s *Server) orderReceived(w http.ResponseWriter, r *http.Request) {
	s.customerOrderStep(w, r, s.Commerce.Received)
}

func (s *Server) customerOrderStep(w http.ResponseWriter, r *http.Request,
	f func(context.Context, uuid.UUID, uuid.UUID) (*commerce.Order, error)) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	o, err := f(r.Context(), platform.UserFrom(r.Context()), id)
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, orderJSON(o))
}

func writeOrders(w http.ResponseWriter, list []commerce.Order) {
	out := make([]map[string]any, len(list))
	for i := range list {
		out[i] = orderJSON(&list[i])
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func orderJSON(o *commerce.Order) map[string]any {
	items := make([]map[string]any, len(o.Items))
	for i, it := range o.Items {
		items[i] = map[string]any{"product_id": it.ProductID, "title": it.Title, "unit_price": it.UnitPrice,
			"quantity": it.Quantity, "line_total": it.LineTotal()}
	}
	m := map[string]any{
		"id": o.ID, "number": o.Number, "status": o.Status, "channel": o.Channel, "merchant_id": o.MerchantID,
		"shop_name": o.ShopName, "items": items, "subtotal": o.Subtotal, "delivery_fee": o.DeliveryFee, "total": o.Total,
		"delivery_address": o.DeliveryAddress, "note": o.Note, "payment_intent_id": o.PaymentIntentID,
		"payment_status": o.PaymentStatus, "cancel_reason": o.CancelReason, "cancel_note": o.CancelNote,
		"created_at": o.CreatedAt, "placed_at": o.PlacedAt, "accepted_at": o.AcceptedAt, "ready_at": o.ReadyAt,
		"completed_at": o.CompletedAt, "cancelled_at": o.CancelledAt, "accept_by": o.AcceptBy, "overdue": o.Overdue, "in_review": o.InReview,
	}
	if o.HandoverCode != "" {
		m["handover_code"] = o.HandoverCode
	}
	if o.CustomerPhone != "" {
		m["customer"] = map[string]any{"name": o.CustomerName, "phone": o.CustomerPhone}
	}
	return m
}

// ---------------------------------------------------------------------------
// Shops
// ---------------------------------------------------------------------------

func (s *Server) getShopSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	sh, err := s.Commerce.ShopSettings(r.Context(), platform.UserFrom(r.Context()), id)
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, shopJSON(sh, true))
}

func (s *Server) putShopSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Open         bool         `json:"open"`
		Pickup       bool         `json:"pickup"`
		Delivery     bool         `json:"delivery"`
		DeliveryFee  *money.Money `json:"delivery_fee"`
		DeliveryArea string       `json:"delivery_area"`
		About        string       `json:"about"`
	}
	if !decode(w, r, &body) {
		return
	}
	u := commerce.ShopUpdate{Open: body.Open, Pickup: body.Pickup, Delivery: body.Delivery, DeliveryFee: money.New(0, money.ETB),
		DeliveryArea: body.DeliveryArea, About: body.About}
	if body.DeliveryFee != nil {
		u.DeliveryFee = *body.DeliveryFee
	}
	sh, err := s.Commerce.UpdateShop(r.Context(), platform.UserFrom(r.Context()), id, u)
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, shopJSON(sh, true))
}

type productBody struct {
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Category    string       `json:"category"`
	Price       *money.Money `json:"price"`
	Stock       *int         `json:"stock"`
	Status      string       `json:"status"`
}

func (b productBody) input() commerce.ProductInput {
	in := commerce.ProductInput{Title: b.Title, Description: b.Description, Category: b.Category, Stock: b.Stock, Status: b.Status}
	if b.Price != nil {
		in.Price = *b.Price
	}
	return in
}

func (s *Server) createProduct(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body productBody
	if !decode(w, r, &body) {
		return
	}
	p, err := s.Commerce.CreateProduct(r.Context(), platform.UserFrom(r.Context()), id, body.input())
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, productJSON(*p))
}

func (s *Server) putProduct(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	pid, err := uuid.Parse(r.PathValue("product"))
	if err != nil {
		s.failCommerce(w, commerce.ErrNotFound)
		return
	}
	var body productBody
	if !decode(w, r, &body) {
		return
	}
	p, err := s.Commerce.UpdateProduct(r.Context(), platform.UserFrom(r.Context()), id, pid, body.input())
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, productJSON(*p))
}

func (s *Server) listShopOrders(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	active := true
	switch r.URL.Query().Get("state") {
	case "", "active":
	case "done":
		active = false
	default:
		platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details",
			"state must be active or done"))
		return
	}
	list, err := s.Commerce.ShopOrders(r.Context(), platform.UserFrom(r.Context()), id, active, 100)
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	writeOrders(w, list)
}

// shopOrderAction: accept, ready, on_the_way, hand_over {code}, cancel {note}.
func (s *Server) shopOrderAction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	oid, err := uuid.Parse(r.PathValue("order"))
	if err != nil {
		s.failCommerce(w, commerce.ErrNotFound)
		return
	}
	var body struct {
		Code string `json:"code"`
		Note string `json:"note"`
	}
	if !decode(w, r, &body) {
		return
	}
	user := platform.UserFrom(r.Context())
	var o *commerce.Order
	switch a := r.PathValue("action"); a {
	case "accept", "ready", "on_the_way":
		o, err = s.Commerce.Advance(r.Context(), user, id, oid, a)
	case "hand_over":
		o, err = s.Commerce.HandOver(r.Context(), user, id, oid, body.Code)
	case "cancel":
		o, err = s.Commerce.CancelByShop(r.Context(), user, id, oid, body.Note)
	default:
		err = commerce.ErrNotFound
	}
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, orderJSON(o))
}

func (s *Server) failCommerce(w http.ResponseWriter, err error) {
	var v *commerce.ValidationError
	var c *commerce.ConflictError
	switch {
	case errors.As(err, &v):
		platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details", v.Msg))
	case errors.As(err, &c):
		platform.WriteProblem(w, platform.NewProblem(http.StatusConflict, "conflict", "Can't do that now", c.Msg))
	case errors.Is(err, commerce.ErrWrongCode):
		platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "wrong_code", "Wrong code",
			"That isn't the code for this order. Ask the customer to show it again in their app."))
	case errors.Is(err, commerce.ErrNotFound):
		platform.WriteProblem(w, platform.NewProblem(http.StatusNotFound, "not_found", "Not found", ""))
	case errors.Is(err, commerce.ErrForbidden):
		platform.WriteProblem(w, platform.NewProblem(http.StatusForbidden, "permission_denied", "Not allowed",
			"Your role at this shop doesn't allow this. Ask the shop's owner."))
	default:
		s.fail(w, err) // payment limits, inactive account and everything else
	}
}

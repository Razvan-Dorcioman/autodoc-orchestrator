package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// maxQty caps the quantity of one order.
const maxQty = 10

// Order is a placed order.
type Order struct {
	ID  int `json:"id"`
	Qty int `json:"qty"`
}

// CreateOrder is the body of POST /orders.
type CreateOrder struct {
	Qty int `json:"qty"`
}

// Note is the body of PUT /notes and its response.
type Note struct {
	Text string `json:"text"`
}

// orders dispatches on the method, as routers written before Go 1.22 do.
func orders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		listOrders(w, r)
	case http.MethodPost:
		createOrder(w, r)
		return
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET or POST")
	}
}

func listOrders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []Order{})
}

func createOrder(w http.ResponseWriter, r *http.Request) {
	var in CreateOrder
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "the body is not a JSON order")
		return
	}
	if err := checkQty(in.Qty); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_qty", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, Order{Qty: in.Qty})
}

func checkQty(n int) error {
	if n < 1 || n > maxQty {
		return errors.New("qty must be between 1 and 10")
	}
	return nil
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// notes handles every method itself and accepts only PUT.
func notes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use PUT")
		return
	}
	data, err := io.ReadAll(r.Body)
	var n Note
	if err == nil {
		err = json.Unmarshal(data, &n)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "the body is not a JSON note")
		return
	}
	writeJSON(w, http.StatusOK, n)
}

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/llm"
	"github.com/j0sehernan/dispute-copilot/internal/psp"
)

// Records is what the tools read from. psp.Client implements it.
type Records interface {
	Transaction(ctx context.Context, id string) (json.RawMessage, error)
	Order(ctx context.Context, id string) (json.RawMessage, error)
	Shipment(ctx context.Context, orderID string) (json.RawMessage, error)
	CustomerHistory(ctx context.Context, customerID string) (json.RawMessage, error)
}

type idKind string

const (
	idTransaction idKind = "transaction"
	idOrder       idKind = "order"
	idCustomer    idKind = "customer"
)

type tool struct {
	def    llm.Tool
	source domain.EvidenceSource
	arg    string // name of the single id argument
	kind   idKind // which kind of id the argument must be
	fetch  func(Records, context.Context, string) (json.RawMessage, error)
}

// The agent can use these four tools and nothing else.
var tools = map[string]tool{
	"get_transaction": {
		def: llm.Tool{
			Name:        "get_transaction",
			Description: "Get the payment record: authorization, 3-D Secure, AVS and CVV results, order_id and customer_id.",
			Parameters:  idParams("transaction_id", "Transaction id, for example txn_8F2K1Q"),
		},
		source: domain.EvidenceTransaction, arg: "transaction_id", kind: idTransaction,
		fetch: Records.Transaction,
	},
	"get_order": {
		def: llm.Tool{
			Name:        "get_order",
			Description: "Get the order: items, customer, billing and shipping address, IP, device and accepted terms.",
			Parameters:  idParams("order_id", "Order id, for example ord_10421"),
		},
		source: domain.EvidenceOrder, arg: "order_id", kind: idOrder,
		fetch: Records.Order,
	},
	"get_shipping": {
		def: llm.Tool{
			Name:        "get_shipping",
			Description: "Get the carrier record of an order: carrier, tracking number, status, delivery address and proof of delivery.",
			Parameters:  idParams("order_id", "Order id, for example ord_10421"),
		},
		source: domain.EvidenceShipping, arg: "order_id", kind: idOrder,
		fetch: Records.Shipment,
	},
	"get_customer_history": {
		def: llm.Tool{
			Name:        "get_customer_history",
			Description: "Get the customer's earlier orders, refunds, returns, disputes and support contacts.",
			Parameters:  idParams("customer_id", "Customer id, for example cus_3021"),
		},
		source: domain.EvidenceCustomerHistory, arg: "customer_id", kind: idCustomer,
		fetch: Records.CustomerHistory,
	},
}

func idParams(name, desc string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"type":"object","properties":{%q:{"type":"string","description":%q}},"required":[%q],"additionalProperties":false}`,
		name, desc, name))
}

func toolDefs() []llm.Tool {
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)
	defs := make([]llm.Tool, 0, len(names))
	for _, n := range names {
		defs = append(defs, tools[n].def)
	}
	return defs
}

// scope tracks which ids the agent is allowed to look up. It starts with the
// disputed transaction and grows only with ids found in records already
// returned. A prompt injection in the notice cannot make the agent read
// another customer's data.
type scope struct {
	mu  sync.Mutex
	ids map[idKind]map[string]bool
}

func newScope(transactionID string) *scope {
	return &scope{ids: map[idKind]map[string]bool{
		idTransaction: {transactionID: true},
		idOrder:       {},
		idCustomer:    {},
	}}
}

func (s *scope) allowed(kind idKind, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[kind][id]
}

// learn adds the ids a record links to.
func (s *scope) learn(src domain.EvidenceSource, data json.RawMessage) {
	var doc struct {
		OrderID    string `json:"order_id"`
		CustomerID string `json:"customer_id"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if doc.OrderID != "" && src == domain.EvidenceTransaction {
		s.ids[idOrder][doc.OrderID] = true
	}
	if doc.CustomerID != "" && (src == domain.EvidenceTransaction || src == domain.EvidenceOrder) {
		s.ids[idCustomer][doc.CustomerID] = true
	}
}

func (s *scope) pending(have domain.EvidenceSet) []pendingCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []pendingCall
	add := func(name string, kind idKind) {
		t := tools[name]
		if _, done := have[t.source]; done {
			return
		}
		for id := range s.ids[kind] {
			out = append(out, pendingCall{name: name, id: id})
			return
		}
	}
	add("get_transaction", idTransaction)
	add("get_order", idOrder)
	add("get_shipping", idOrder)
	add("get_customer_history", idCustomer)
	return out
}

type pendingCall struct {
	name string
	id   string
}

// toolResult is what we send back to the model and what we keep as
// evidence.
type toolResult struct {
	name    string
	source  domain.EvidenceSource
	id      string
	found   bool
	data    json.RawMessage
	content string // message for the model
	err     error  // unexpected failure (network, 5xx)
	refused string // guardrail reason, if the call was not allowed
}

func runTool(ctx context.Context, recs Records, sc *scope, call llm.ToolCall) toolResult {
	t, ok := tools[call.Name]
	if !ok {
		return toolResult{name: call.Name, refused: "unknown_tool",
			content: `{"error":"unknown tool. Use only get_transaction, get_order, get_shipping or get_customer_history"}`}
	}

	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return toolResult{name: call.Name, source: t.source, refused: "bad_arguments",
			content: `{"error":"arguments must be a JSON object"}`}
	}
	id, _ := args[t.arg].(string)
	if id == "" || len(args) != 1 {
		return toolResult{name: call.Name, source: t.source, refused: "bad_arguments",
			content: fmt.Sprintf(`{"error":"expected exactly one argument: %s"}`, t.arg)}
	}
	if !sc.allowed(t.kind, id) {
		return toolResult{name: call.Name, source: t.source, id: id, refused: "id_out_of_scope",
			content: `{"error":"this id is not linked to the disputed transaction"}`}
	}

	data, err := t.fetch(recs, ctx, id)
	switch {
	case errors.Is(err, psp.ErrNotFound):
		return toolResult{name: call.Name, source: t.source, id: id, found: false,
			content: `{"error":"not_found"}`}
	case err != nil:
		return toolResult{name: call.Name, source: t.source, id: id, err: err,
			content: `{"error":"lookup failed, try again later"}`}
	}
	sc.learn(t.source, data)
	return toolResult{name: call.Name, source: t.source, id: id, found: true, data: data, content: string(data)}
}

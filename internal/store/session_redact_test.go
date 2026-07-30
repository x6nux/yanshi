package store

import (
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/secrets"
)

func TestStore_RedactsAllWritePaths(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := secrets.NewRedactor()
	secret := "sk-db-leak-12345"
	r.Register(secret)
	st.SetRedactor(r)

	id, err := st.CreateSession("session with " + secret)
	if err != nil {
		t.Fatal(err)
	}
	var title string
	if err := st.DB.QueryRow("SELECT title FROM sessions WHERE id = ?", id).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(title, secret) {
		t.Fatalf("CreateSession leaked: %q", title)
	}

	if err := st.AppendMessage(id, 0, "user", "message with " + secret); err != nil {
		t.Fatal(err)
	}
	msgs, err := st.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msgs[len(msgs)-1].Content, secret) {
		t.Fatalf("AppendMessage leaked: %q", msgs[len(msgs)-1].Content)
	}

	if err := st.UpdateSessionTitle(id, "title with " + secret); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRow("SELECT title FROM sessions WHERE id = ?", id).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(title, secret) {
		t.Fatalf("UpdateSessionTitle leaked: %q", title)
	}
}

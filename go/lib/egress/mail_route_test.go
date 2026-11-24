package egress

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// A mail client goes through the same registrar as every egress client:
// the project's egress package does not know the difference.
func TestRegisterFromEnvRoutesAMailClient(t *testing.T) {
	fd := compile("clients/mailer/mailer.proto", `syntax = "proto3";
package clients.mailer;
import "w17/egress.proto";
import "w17/mail.proto";
option (w17.egress_client) = { name: "mailer", mail: true };
service MailerApi { rpc Send(w17.MailMessage) returns (w17.MailReceipt); }
`)
	svc := fd.Services().ByName("MailerApi")
	if !IsMail(svc) {
		t.Fatal("not a mail client")
	}
	conn := inprocgrpc.New()
	if err := RegisterFromEnv(conn, svc, "W17_CLIENT_MAILER_DSN", func(string) (string, bool) { return "", false }); err != nil {
		t.Fatal(err)
	}
	err := conn.Invoke(context.Background(), "/clients.mailer.MailerApi/Send",
		&w17pb.MailMessage{From: "a@example.com", To: []string{"b@example.com"}, Text: "x"}, &w17pb.MailReceipt{})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "W17_CLIENT_MAILER_DSN") {
		t.Fatalf("%v", err)
	}
	// Its DSN is a mail one: an HTTP base URL fails the boot.
	err = RegisterFromEnv(inprocgrpc.New(), svc, "W17_CLIENT_MAILER_DSN", func(string) (string, bool) { return "https://api.example.com", true })
	if err == nil || !strings.Contains(err.Error(), "W17_CLIENT_MAILER_DSN") {
		t.Fatalf("%v", err)
	}
	if err := Register(inprocgrpc.New(), svc, &DSN{}); err == nil {
		t.Fatal("Register took a mail client with an HTTP DSN")
	}
}

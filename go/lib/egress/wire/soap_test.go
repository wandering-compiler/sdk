package wire

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

var featureSoap = compileFixture("feature_soap")

func soapCall(t *testing.T, method string) *w17pb.EgressCall {
	t.Helper()
	return Call(featureSoap.Services().ByName("FeatureSoapApi").Methods().ByName(protoreflect.Name(method)))
}

func TestMarshalSOAP_Envelope(t *testing.T) {
	m := dynamicpb.NewMessage(mdIn(t, featureSoap, "StatusRequest"))
	if err := protojson.Unmarshal([]byte(`{"dic":["CZ123","CZ4<5"]}`), m); err != nil {
		t.Fatal(err)
	}
	b, err := MarshalSOAP(soapCall(t, "GetStatus"), m)
	if err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:ns1="urn:feature">` +
		`<soapenv:Header/><soapenv:Body>` +
		`<ns1:StatusRequest><ns1:dic>CZ123</ns1:dic><ns1:dic>CZ4&lt;5</ns1:dic></ns1:StatusRequest>` +
		`</soapenv:Body></soapenv:Envelope>`
	if string(b) != want {
		t.Errorf("envelope:\n got %s\nwant %s", b, want)
	}
}

func TestMarshalSOAP_AttributesOneofAndSOAP12(t *testing.T) {
	m := dynamicpb.NewMessage(mdIn(t, featureSoap, "Payer"))
	if err := protojson.Unmarshal([]byte(`{"dic":"CZ1","nespolehlivyPlatce":"RELIABILITY_NE","account":[{"standardniUcet":{"cislo":"19-1","kodBanky":"0100"}},{"nestandardniUcet":"DE89"}]}`), m); err != nil {
		t.Fatal(err)
	}
	w := &xmlWriter{prefix: map[string]string{}}
	w.collect("urn:feature")
	if err := w.element("urn:feature", "payer", m); err != nil {
		t.Fatal(err)
	}
	want := `<ns1:payer dic="CZ1" nespolehlivyPlatce="NE">` +
		`<ns1:account><ns1:standardniUcet cislo="19-1" kodBanky="0100"></ns1:standardniUcet></ns1:account>` +
		`<ns1:account><ns1:nestandardniUcet>DE89</ns1:nestandardniUcet></ns1:account></ns1:payer>`
	if w.buf.String() != want {
		t.Errorf("payer:\n got %s\nwant %s", w.buf.String(), want)
	}

	call := &w17pb.EgressCall{RequestEncoding: w17pb.EgressEncoding_EGRESS_ENCODING_SOAP12, SoapAction: "urn:a"}
	if ct := SOAPContentType(call); ct != `application/soap+xml; charset=utf-8; action="urn:a"` {
		t.Errorf("SOAP 1.2 content type %q", ct)
	}
	b, err := MarshalSOAP(call, dynamicpb.NewMessage(mdIn(t, featureSoap, "RegisterRequest")))
	if err != nil || !strings.Contains(string(b), `xmlns:soapenv="http://www.w3.org/2003/05/soap-envelope"`) {
		t.Errorf("SOAP 1.2 envelope %s (%v)", b, err)
	}
}

func TestUnmarshalSOAP(t *testing.T) {
	raw := `<?xml version="1.0"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/">
  <soapenv:Body>
    <StatusResponse xmlns="urn:feature" xmlns:x="urn:other">
      <status statusCode="0" statusText="OK"/>
      <payer dic="CZ1" nespolehlivyPlatce="ANO" cisloFu="461">
        <checkedAt>2026-11-15T10:00:00</checkedAt>
        <account><standardniUcet cislo="19-1" kodBanky="0100"/></account>
        <account><nestandardniUcet> DE89 </nestandardniUcet></account>
        <x:unknown><deep/></x:unknown>
      </payer>
      <payer dic="CZ2" nespolehlivyPlatce="SOMETHING_NEW"/>
    </StatusResponse>
  </soapenv:Body>
</soapenv:Envelope>`
	m := dynamicpb.NewMessage(mdIn(t, featureSoap, "StatusResponse"))
	if err := UnmarshalSOAP([]byte(raw), m); err != nil {
		t.Fatal(err)
	}
	got, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	for _, want := range []string{
		`"status_code":"0"`, `"status_text":"OK"`,
		`"dic":"CZ1"`, `"nespolehlivy_platce":"RELIABILITY_ANO"`, `"cislo_fu":"461"`,
		`"checked_at":"2026-11-15T10:00:00Z"`,
		`"standardni_ucet":{"cislo":"19-1","kod_banky":"0100"}`, `"nestandardni_ucet":"DE89"`,
		`"dic":"CZ2"`,
	} {
		if !strings.Contains(strings.ReplaceAll(string(got), " ", ""), want) {
			t.Errorf("decoded %s\n missing %s", got, want)
		}
	}
	// SOMETHING_NEW is not in the schema: the zero value, which JSON omits.
	if strings.Count(string(got), "nespolehlivy_platce") != 1 {
		t.Errorf("an unknown enum value is the zero value: %s", got)
	}
	if err := UnmarshalSOAP([]byte(`<a/>`), m); err == nil {
		t.Error("a document without a Body decodes")
	}
}

func TestParseFault(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      Fault
		ok        bool
	}{
		{"1.1", `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault>
			<faultcode>s:Client</faultcode><faultstring>bad DIC</faultstring>
			<detail><e>CZ? is not a DIC</e></detail></s:Fault></s:Body></s:Envelope>`,
			Fault{Code: "Client", Reason: "bad DIC", Detail: "CZ? is not a DIC"}, true},
		{"1.2", `<env:Envelope xmlns:env="http://www.w3.org/2003/05/soap-envelope"><env:Body><env:Fault>
			<env:Code><env:Value>env:Receiver</env:Value><env:Subcode><env:Value>x:Busy</env:Value></env:Subcode></env:Code>
			<env:Reason><env:Text xml:lang="cs">nedostupné</env:Text></env:Reason></env:Fault></env:Body></env:Envelope>`,
			Fault{Code: "Receiver", Reason: "nedostupné"}, true},
		{"no fault", `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><r/></s:Body></s:Envelope>`, Fault{}, false},
		{"not xml", `Service Unavailable`, Fault{}, false},
	} {
		f, ok := ParseFault([]byte(tc.raw))
		if ok != tc.ok || f != tc.want {
			t.Errorf("%s: %+v %v, want %+v %v", tc.name, f, ok, tc.want, tc.ok)
		}
	}
	if !(Fault{Code: "Sender"}).ClientFault() || (Fault{Code: "Server"}).ClientFault() {
		t.Error("ClientFault table is wrong")
	}
}

func TestParseXSDDateTime(t *testing.T) {
	for _, s := range []string{"2026-11-15T10:00:00Z", "2026-11-15T11:00:00+01:00", "2026-11-15T10:00:00"} {
		got, err := parseXSDDateTime(s)
		if err != nil || !got.Equal(time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)) {
			t.Errorf("%s: %v %v", s, got, err)
		}
	}
}

func TestUnmarshalSOAP_NilEmptyAndText(t *testing.T) {
	raw := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><s:Body>
		<StatusResponse xmlns="urn:feature">
		  <payer dic="CZ1" cisloFu=""><checkedAt xsi:nil="true"/></payer>
		  <payer dic="CZ2"><checkedAt></checkedAt></payer>
		</StatusResponse></s:Body></s:Envelope>`
	m := dynamicpb.NewMessage(mdIn(t, featureSoap, "StatusResponse"))
	if err := UnmarshalSOAP([]byte(raw), m); err != nil {
		t.Fatal(err)
	}
	got, _ := protojson.Marshal(m)
	if strings.Contains(string(got), "checkedAt") || strings.Contains(string(got), "cisloFu") {
		t.Errorf("a nil or empty value is no value: %s", got)
	}

	amount := dynamicpb.NewMessage(mdIn(t, featureSoap, "Amount"))
	if err := protojson.Unmarshal([]byte(`{"value":"12.50","currency":"CZK"}`), amount); err != nil {
		t.Fatal(err)
	}
	w := &xmlWriter{prefix: map[string]string{}}
	if err := w.element("", "amount", amount); err != nil {
		t.Fatal(err)
	}
	if w.buf.String() != `<amount currency="CZK">12.50</amount>` {
		t.Errorf("simpleContent: %s", w.buf.String())
	}
	back := dynamicpb.NewMessage(mdIn(t, featureSoap, "Amount"))
	d := xml.NewDecoder(strings.NewReader(w.buf.String()))
	tok, _ := d.Token()
	if err := decodeInto(d, tok.(xml.StartElement), back); err != nil || !proto.Equal(back, amount) {
		t.Errorf("simpleContent round trip: %v %v", back, err)
	}
}

// A value keeps what its XSD type's whiteSpace facet keeps: an xsd:string
// arrives padded as the service padded it (a register's fixed-width column),
// a collapse type and a non-string kind are trimmed and folded, and a
// normalizedString attribute has its line breaks turned to spaces.
func TestUnmarshalSOAP_WhitespaceFollowsTheFacet(t *testing.T) {
	raw := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>
		<StatusResponse xmlns="urn:feature">
		  <status statusCode="0" statusText="MINISTERSTVO FINANCÍ   "/>
		  <payer dic=" CZ1 " cisloFu=" 461 ">
		    <account><nestandardniUcet>
		      DE89   3704 </nestandardniUcet></account>
		    <account><standardniUcet cislo="19-1" kodBanky="0100" nazevBanky="Česká&#10;národní	banka"/></account>
		    <balance currency="CZK">
		      12.50 </balance>
		  </payer>
		</StatusResponse></s:Body></s:Envelope>`
	m := dynamicpb.NewMessage(mdIn(t, featureSoap, "StatusResponse"))
	if err := UnmarshalSOAP([]byte(raw), m); err != nil {
		t.Fatal(err)
	}
	got, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	for _, want := range []string{
		`"status_text":"MINISTERSTVO FINANCÍ   "`, // xsd:string: preserve
		`"dic":" CZ1 "`,                       // xsd:string attribute: preserve
		`"cislo_fu":"461"`,                    // a number: collapsed
		`"nestandardni_ucet":"DE89 3704"`,     // a collapse facet
		`"nazev_banky":"Česká národní banka"`, // normalizedString: replace
		`"value":"12.50"`,                     // xsd:decimal text: collapsed
	} {
		if !strings.Contains(strings.ReplaceAll(string(got), `": "`, `":"`), want) {
			t.Errorf("decoded %s\n missing %s", got, want)
		}
	}
}

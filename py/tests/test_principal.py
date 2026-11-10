import base64

import pytest

from w17_business import paging, principal
from w17_business.context import Context, Metadata


def envelope(msg, uid="u-1", **kw):
    return base64.b64encode(msg("AuthResp")(user_id=uid, **kw).SerializeToString()).decode()


# --- forward_to_outgoing: the exact key set, replace semantics ---------------


def test_forward_relays_exactly_the_gateway_stamps(msg):
    incoming = {
        "x-w17-user": envelope(msg),
        "x-w17-scope-org_id": "o-1",
        "w17-label-tenant": "t-1",
        "x-w17-device": "dev",
        "x-w17-dpop-jkt": "jkt",
        "x-w17-data-scope": "org",
        "x-w17-audience": "public",
        "x-w17-membership-kinds": "member",
        "x-w17-org-from": "host",
        "x-w17-host": "example.test",
        # never relayed:
        "authorization": "Bearer secret",
        "x-w17-paging-limit": "5",
        "w17-language": "cs",
        "traceparent": "00-abc-def-01",
        "w17-tx-id": "tx-9",
        "x-w17-surface": "rest",
        "x-custom": "c",
    }
    out = principal.forward_to_outgoing(Context(incoming=incoming)).outgoing
    relayed = {
        "x-w17-user", "x-w17-scope-org_id", "w17-label-tenant", "x-w17-device", "x-w17-dpop-jkt",
        "x-w17-data-scope", "x-w17-audience", "x-w17-membership-kinds", "x-w17-org-from", "x-w17-host",
    }
    assert set(out) == relayed
    for k in relayed:
        assert out.get_all(k) == (incoming[k],)


def test_forward_replaces_the_whole_relayed_set_and_keeps_other_outgoing_keys(msg):
    ctx = Context(
        incoming={"x-w17-user": envelope(msg, "new"), "x-w17-scope-org_id": "o-new"},
        outgoing={
            "x-w17-user": "stale",
            "x-w17-scope-org_id": "o-stale",
            "x-w17-scope-team_id": "t-stale",  # outgoing-only relayed key: dropped
            "w17-label-x": "stale",
            "w17-tx-id": "tx-1",  # not relayed: kept
            "x-w17-paging-limit": "3",  # not relayed: kept
        },
    )
    out = principal.forward_to_outgoing(ctx).outgoing
    assert out.get_all("x-w17-user") == (envelope(msg, "new"),)
    assert out.get_all("x-w17-scope-org_id") == ("o-new",)
    assert "x-w17-scope-team_id" not in out
    assert "w17-label-x" not in out
    assert out.get_all("w17-tx-id") == ("tx-1",)
    assert out.get_all("x-w17-paging-limit") == ("3",)


def test_forward_without_incoming_principal_returns_ctx_unchanged():
    ctx = Context(incoming={"w17-language": "cs"}, outgoing={"x-w17-user": "gateway-stamped"})
    assert principal.forward_to_outgoing(ctx) is ctx


def test_forward_is_idempotent_and_never_duplicates(msg):
    ctx = Context(incoming={"x-w17-user": envelope(msg), "x-w17-scope-org_id": "o"})
    once = principal.forward_to_outgoing(ctx)
    twice = principal.forward_to_outgoing(once)
    assert twice.outgoing == once.outgoing
    assert twice.outgoing.get_all("x-w17-scope-org_id") == ("o",)


def test_forward_keeps_every_value_of_a_multivalued_key():
    ctx = Context(incoming=[("w17-label-a", "1"), ("w17-label-a", "2")])
    assert principal.forward_to_outgoing(ctx).outgoing.get_all("w17-label-a") == ("1", "2")


def test_metadata_keys_are_case_insensitive():
    md = Metadata({"X-W17-User": "e"})
    assert md.get_all("x-w17-user") == ("e",)
    assert principal.is_relayed_key("x-w17-user")


# --- readers ------------------------------------------------------------------


def test_user_id_fast_path_then_envelope(msg):
    principal.set_auth_resp_type(msg("AuthResp"))
    assert principal.user_id(Context(incoming={"x-w17-scope-user_id": "fast"})) == "fast"
    assert principal.user_id(Context(incoming={"x-w17-user": envelope(msg, "env")})) == "env"
    assert principal.user_id(Context(incoming={"x-w17-user": envelope(msg, "x")}), msg("AuthResp")) == "x"


@pytest.mark.parametrize(
    "incoming",
    [
        {},
        {"x-w17-user": ""},
        {"x-w17-user": "!!not base64!!"},
        {"x-w17-user": base64.b64encode(b"\xff\xff\xff").decode()},
    ],
)
def test_user_id_fails_closed(msg, incoming):
    principal.set_auth_resp_type(msg("AuthResp"))
    with pytest.raises(principal.NoPrincipalError):
        principal.user_id(Context(incoming=incoming))


def test_user_id_empty_user_id_is_no_principal(msg):
    principal.set_auth_resp_type(msg("AuthResp"))
    with pytest.raises(principal.NoPrincipalError):
        principal.user_id(Context(incoming={"x-w17-user": envelope(msg, "")}))


def test_envelope_decodes_the_whole_auth_resp(msg):
    who = principal.envelope(Context(incoming={"x-w17-user": envelope(msg, "u", scopes={"org_id": "o"})}), msg("AuthResp"))
    assert who.user_id == "u" and who.scopes["org_id"] == "o"


def test_scope_label_device_data_scope():
    ctx = Context(incoming={
        "x-w17-scope-org_id": "o",
        "w17-label-kind": "bot",
        "x-w17-device": "d",
        "x-w17-data-scope": "org",
    })
    assert principal.scope(ctx, "org_id") == "o"
    assert principal.scope(ctx, "team_id") is None
    assert principal.label(ctx, "kind") == "bot"
    assert principal.label(ctx, "other") is None
    assert principal.device_id(ctx) == "d"
    assert principal.data_scope(ctx) == "org"


def test_device_id_needs_exactly_one_value():
    assert principal.device_id(Context(incoming=[("x-w17-device", "a"), ("x-w17-device", "b")])) is None


@pytest.mark.parametrize("vals,want", [((), ""), (("all",),"all"), (("weird",), "owner"), (("org", "all"), "owner")])
def test_data_scope_unknown_or_several_is_the_narrowest(vals, want):
    ctx = Context(incoming=[("x-w17-data-scope", v) for v in vals])
    assert principal.data_scope(ctx) == want


# --- act_as -------------------------------------------------------------------


def test_act_as_replaces_the_principal_on_both_sides(msg):
    ctx = Context(
        incoming={"x-w17-user": envelope(msg, "caller"), "x-w17-scope-org_id": "o-caller", "w17-language": "cs"},
        outgoing={"x-w17-scope-team_id": "t", "w17-tx-id": "tx"},
    )
    acted = principal.act_as(ctx, msg("AuthResp")(user_id="person"), {"org_id": "o-person"})
    assert "x-w17-user" not in acted.incoming and acted.incoming.get_all("w17-language") == ("cs",)
    out = acted.outgoing
    assert out.get_all("x-w17-scope-org_id") == ("o-person",)
    assert "x-w17-scope-team_id" not in out
    assert out.get_all("w17-tx-id") == ("tx",)
    # A later forward cannot put the caller back.
    assert principal.forward_to_outgoing(acted).outgoing == out
    decoded = msg("AuthResp").FromString(base64.b64decode(out.first("x-w17-user")))
    assert decoded.user_id == "person"


@pytest.mark.parametrize("scopes", [{"": "v"}, {"org_id": ""}, {" org_id": "v"}])
def test_act_as_refuses_malformed_input(msg, scopes):
    with pytest.raises(ValueError):
        principal.act_as(Context(), msg("AuthResp")(user_id="u"), scopes)
    with pytest.raises(ValueError):
        principal.act_as(Context(), msg("AuthResp")(user_id=""))


def test_gateway_owned_key():
    for k in ("x-w17-anything", "W17-LABEL-x", "w17-tx-id", "w17-conn-id"):
        assert principal.is_gateway_owned_key(k)
    for k in ("w17-org", "w17-tx-id-hint", "authorization"):
        assert not principal.is_gateway_owned_key(k)


# --- paging.forward -----------------------------------------------------------


def test_paging_forward_copies_incoming_onto_outgoing_only():
    ctx = Context(incoming={paging.BOUNDARIES_MD_KEY: "c", paging.LIMIT_MD_KEY: "25", paging.TOTAL_MD_KEY: "1",
                            "x-w17-user": "e"})
    out = paging.forward(ctx).outgoing
    assert dict(out) == {paging.BOUNDARIES_MD_KEY: ("c",), paging.LIMIT_MD_KEY: ("25",), paging.TOTAL_MD_KEY: ("1",)}
    assert len(ctx.outgoing) == 0  # the original context is untouched


def test_paging_forward_replaces_stale_outgoing_as_a_set():
    ctx = Context(incoming={paging.LIMIT_MD_KEY: "50"},
                  outgoing={paging.BOUNDARIES_MD_KEY: "stale", paging.LIMIT_MD_KEY: "10", "w17-language": "cs"})
    out = paging.forward(ctx).outgoing
    assert out.get_all(paging.LIMIT_MD_KEY) == ("50",)
    assert paging.BOUNDARIES_MD_KEY not in out
    assert out.get_all("w17-language") == ("cs",)


def test_paging_forward_nothing_when_absent():
    ctx = Context(incoming={"x-w17-user": "e"}, outgoing={paging.LIMIT_MD_KEY: "10"})
    assert paging.forward(ctx) is ctx


def test_a_cancel_before_the_deadline_stays_a_cancel_after_it_passes():
    # Go keeps the first cause: a context cancelled before its deadline reads
    # "context canceled" after the deadline passes, and one whose deadline
    # had passed when it was cancelled (the server cancels a call that ran
    # out of time) reads "context deadline exceeded".
    import time

    ctx = Context().with_timeout(0.01)
    ctx.cancel()
    time.sleep(0.025)
    assert ctx.err() == "context canceled"

    parent = Context().with_timeout(0.005)
    child = parent.with_cancel()
    time.sleep(0.02)
    parent.cancel()
    assert child.err() == "context deadline exceeded"
    assert parent.err() == "context deadline exceeded"

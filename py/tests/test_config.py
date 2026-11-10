import json
from pathlib import Path

import pyrage
import pytest

from w17_business import secrets
from w17_business.config import Config, ConfigError
from w17_business.manifest import ConfigKey, ManifestError, from_dict, load

KEYS = (
    ConfigKey(name="greeting", env="N_GREETING", default="hello"),
    ConfigKey(name="limit", env="N_LIMIT", type="int", default="10"),
    ConfigKey(name="verbose", env="N_VERBOSE", type="bool", default="false"),
    ConfigKey(name="token", env="N_TOKEN", secret=True),
)


def test_defaults_and_typed_getters(chdir_tmp):
    cfg = Config.load(KEYS, {})
    assert cfg.str("greeting") == "hello"
    assert cfg.int("limit") == 10
    assert cfg.bool("verbose") is False
    assert cfg.secret("token").is_zero()


def test_values_parse_like_go(chdir_tmp):
    cfg = Config.load(KEYS, {"N_GREETING": "hi", "N_LIMIT": "-3", "N_VERBOSE": "1", "N_TOKEN": "s3cret"})
    assert (cfg.str("greeting"), cfg.int("limit"), cfg.bool("verbose")) == ("hi", -3, True)
    assert cfg.secret("token").reveal() == "s3cret"
    assert Config.load(KEYS, {"N_LIMIT": "+4"}).int("limit") == 4


@pytest.mark.parametrize("env,name", [
    ({"N_LIMIT": "ten"}, "N_LIMIT"),
    ({"N_LIMIT": "1.5"}, "N_LIMIT"),
    ({"N_VERBOSE": "yes"}, "N_VERBOSE"),
    ({"N_VERBOSE": "True"}, "N_VERBOSE"),
])
def test_unparseable_value_stops_the_boot_naming_the_var(chdir_tmp, env, name):
    with pytest.raises(ConfigError, match=name):
        Config.load(KEYS, env)


def test_required_key_missing_names_key_and_var(chdir_tmp):
    keys = KEYS + (ConfigKey(name="api_url", env="N_API_URL", required=True),
                   ConfigKey(name="api_key", env="N_API_KEY", secret=True, required=True))
    with pytest.raises(ConfigError, match="'api_url'.*N_API_URL is required"):
        Config.load(keys, {"N_API_KEY": "k"})
    with pytest.raises(ConfigError, match="'api_key'.*N_API_KEY is required"):
        Config.load(keys, {"N_API_URL": "u"})


def test_secret_never_prints(chdir_tmp):
    cfg = Config.load(KEYS, {"N_TOKEN": "s3cret"})
    s = cfg.secret("token")
    assert repr(s) == str(s) == f"{s}" == "***"
    assert "s3cret" not in repr(cfg)


def test_undeclared_or_mistyped_key(chdir_tmp):
    cfg = Config.load(KEYS, {})
    with pytest.raises(KeyError):
        cfg.str("nope")
    with pytest.raises(TypeError):
        cfg.str("limit")
    assert cfg["limit"] == 10


# --- secrets ------------------------------------------------------------------


def test_dotenv_parse_matches_go():
    kv = secrets.parse_dotenv("# c\n\nA=1\n B = two \nC=x=y\r\nNOEQ\n=v\nD=\"q\"\n")
    assert kv == {"A": "1", "B": " two ", "C": "x=y", "D": '"q"'}


def _age(tmp_path, content: str):
    ident = pyrage.x25519.Identity.generate()
    (tmp_path / ".secrets.age").write_bytes(pyrage.encrypt(content.encode(), [ident.to_public()]))
    return str(ident)


def test_chain_env_then_age_then_plain(chdir_tmp):
    key = _age(chdir_tmp, "A=age\nB=age\n")
    (chdir_tmp / ".secrets").write_text("A=plain\nB=plain\nC=plain\n")
    r = secrets.new_resolver({"W17_SECRETS_AGE_KEY": key, "A": "env"})
    assert (r.get("A"), r.get("B"), r.get("C"), r.get("D")) == ("env", "age", "plain", "")


def test_age_key_file_and_comment_lines(chdir_tmp):
    key = _age(chdir_tmp, "A=age\n")
    (chdir_tmp / "key.txt").write_text(f"# created: now\n# public key: x\n{key}\n")
    r = secrets.new_resolver({"W17_SECRETS_AGE_KEY_FILE": str(chdir_tmp / "key.txt")})
    assert r.get("A") == "age"


def test_auto_without_key_skips_the_age_file(chdir_tmp):
    _age(chdir_tmp, "A=age\n")
    assert secrets.new_resolver({}).get("A") == ""


def test_mode_age_requires_a_key(chdir_tmp):
    with pytest.raises(secrets.SecretsError, match="mode=age but no age key"):
        secrets.new_resolver({"W17_SECRETS_MODE": "age"})


def test_wrong_key_fails_loudly(chdir_tmp):
    _age(chdir_tmp, "A=age\n")
    other = str(pyrage.x25519.Identity.generate())
    with pytest.raises(secrets.SecretsError, match="wrong key"):
        secrets.new_resolver({"W17_SECRETS_AGE_KEY": other})


def test_mode_plain_ignores_the_age_file(chdir_tmp):
    key = _age(chdir_tmp, "A=age\n")
    (chdir_tmp / ".secrets").write_text("A=plain\n")
    assert secrets.new_resolver({"W17_SECRETS_MODE": "plain", "W17_SECRETS_AGE_KEY": key}).get("A") == "plain"


def test_unknown_mode(chdir_tmp):
    with pytest.raises(secrets.SecretsError, match="unknown W17_SECRETS_MODE"):
        secrets.new_resolver({"W17_SECRETS_MODE": "vault"})


def test_secret_config_resolves_through_the_chain(chdir_tmp):
    (chdir_tmp / ".secrets").write_text("N_TOKEN=from-file\n")
    assert Config.load(KEYS, {}).secret("token").reveal() == "from-file"


# --- manifest -----------------------------------------------------------------

SPEC_EXAMPLE = Path(__file__).parent / "testdata" / "shop.json"


def test_conformance_manifest_loads():
    from conftest import MANIFEST

    m = load(MANIFEST)
    assert m.domain == "notes" and m.register.module == "conformance_notes.w17"
    assert m.emits[0].fields[2].value == "conformance"
    assert m.emits[0].envelope == "conformance.events.Event"
    assert m.storage.distributed_transactions == "w17.common.distx.W17DistributedTransaction"
    assert m.path(m.descriptor_set).endswith("testdata/descriptors.binpb")
    assert [k.name for k in m.config if k.required] == ["api_token"]


def test_the_specs_example_reads_as_specified():
    # The example manifest of the platform's business-manifest spec, verbatim.
    m = load(SPEC_EXAMPLE)
    assert (m.bundle, m.env_prefix, m.listen_env, m.default_listen_addr) == (
        "shop-business", "SHOP", "SHOP_LISTEN_ADDR", ":50051")
    assert m.shutdown_timeout == 10 and m.descriptor_set == "descriptors.binpb"
    assert (m.register.path, m.register.module, m.register.function) == ("business/shop", "shop.w17", "register_business")
    svc = m.services[0]
    assert svc.name == "shop.core.CheckoutService"
    assert [x.name for x in svc.methods] == ["PlaceOrder", "Watch", "Cancel"]
    assert svc.methods[1].server_streaming and not svc.methods[0].server_streaming
    cfg = {k.name: k for k in m.config}
    # Typed defaults become the text a variable would hold.
    assert (cfg["max_items"].default, cfg["dry_run"].default, cfg["api_key"].secret) == ("25", "true", True)
    assert Config.load(m.config, {}).int("max_items") == 25
    assert Config.load(m.config, {}).bool("dry_run") is True
    assert m.storage.address_env == "SHOP_STORAGE_BACKEND_ADDR"
    assert m.storage.services == ("shop.core.OrderMutation", "shop.core.OrderQuery")
    ev = m.events
    assert ev.nats_dsn_env == "SHOP_EVENTBUS_NATS_DSN" and ev.redis_dsn_env == "" and ev.redis_channels == ()
    e = ev.emits[0]
    assert (e.method, e.event, e.envelope, e.envelope_field, e.channel, e.topic) == (
        "/shop.core.CheckoutService/PlaceOrder", "shop.OrderPlaced", "w17lock.shop.events.Event",
        "shop_order_placed", "default", "order.placed")
    assert [(f.field, f.source, f.path, f.value) for f in e.fields] == [
        ("order_id", "response", ("order_id",), ""),
        ("source", "literal", (), "checkout"),
        ("total", "request", ("cart", "total"), ""),
    ]
    inv = ev.invalidates[0]
    assert (inv.method, inv.topic, inv.group, inv.entities) == (
        "/shop.core.CheckoutService/Cancel", "w17.invalidate", "shop.core", ("Order",))
    assert m.plugin_hooks == ()


def _min(**kw):
    d = {"format": 1, "domain": "d", "env_prefix": "D", "language": "python", "register": {"path": "biz/d"}}
    d.update(kw)
    return d


def test_manifest_format_and_language_are_checked():
    with pytest.raises(ManifestError, match="reads formats 1 and 2"):
        from_dict(_min(format=3))
    assert from_dict(_min(format=2)).companion is None
    with pytest.raises(ManifestError, match="format"):
        from_dict({k: v for k, v in _min().items() if k != "format"})
    with pytest.raises(ManifestError, match="not python"):
        from_dict(_min(language="typescript"))


def test_manifest_defaults_nulls_and_unknown_fields():
    m = from_dict(_min(storage=None, events=None, future=1, config=[{"name": "x"}]))
    assert m.listen_env == "D_LISTEN_ADDR" and m.reflection_env == "D_GRPC_REFLECTION"
    assert m.register.module == "d.w17" and m.register.package == "d"
    assert m.storage is None and m.events is None and m.emits == () and m.invalidates == ()
    assert m.config[0].env == "D_X"
    assert m.extra == {"future": 1}


def test_manifest_required_fields(tmp_path):
    p = tmp_path / "m.json"
    p.write_text(json.dumps({"format": 1, "domain": "d", "env_prefix": "D"}))
    with pytest.raises(ManifestError, match="register is required"):
        load(p)
    p.write_text("{nope")
    with pytest.raises(ManifestError, match="not JSON"):
        load(p)
    with pytest.raises(ManifestError, match="envelope is required"):
        from_dict(_min(events={"envelope": "", "emits": [
            {"method": "/a.S/M", "event": "a.E", "envelope_field": "e", "channel": "c", "topic": "t"}]}))


# The top level the Go companion decodes (bizcompanion.Load), refused as it
# refuses it: the same table in every SDK's unit tests (py, ts, rs).
_TOP_LEVEL_REFUSALS = [
    ({"format": "1"}, "format"),
    ({"format": True}, "format"),
    ({"format": 1.0}, "format"),
    ({"format": 3}, "reads formats 1 and 2"),
    ({"domain": None}, "domain is required"),
    ({"domain": ""}, "domain is required"),
    ({"domain": 5}, "domain must be a string"),
    ({"env_prefix": None}, "env_prefix is required"),
    ({"env_prefix": ""}, "env_prefix is required"),
    ({"env_prefix": ["D"]}, "env_prefix must be a string"),
    ({"bundle": 7}, "bundle must be a string"),
    ({"descriptor_set": {}}, "descriptor_set must be a string"),
    # Keys only the SDKs read, held to their JSON types all the same.
    ({"shutdown_timeout_seconds": "abc"}, "shutdown_timeout_seconds must be a number"),
    ({"shutdown_timeout_seconds": True}, "shutdown_timeout_seconds must be a number"),
    ({"config": [{"name": "x", "required": "false"}]}, r"config\[\]\.required must be true or false"),
    ({"events": {"envelope": "e", "emits": {}}}, r"events\.emits must be a list"),
]


@pytest.mark.parametrize("change,want", _TOP_LEVEL_REFUSALS)
def test_manifest_top_level_is_read_with_gos_rules(change, want):
    d = _min(**change)  # a null is refused as an absent key is
    with pytest.raises(ManifestError, match=want):
        from_dict(d)


def test_manifest_top_level_nulls_take_gos_defaults():
    m = from_dict(_min(bundle=None, descriptor_set=None))
    assert (m.bundle, m.descriptor_set, m.env_prefix) == ("d-business", "descriptors.binpb", "D")


@pytest.mark.parametrize("raw", ["9223372036854775808", "-9223372036854775809", "1" + "0" * 30, "9" * 5000])
def test_int_out_of_int64_range_is_refused_like_go_atoi(chdir_tmp, raw):
    with pytest.raises(ConfigError, match="N_LIMIT.*out of range"):
        Config.load(KEYS, {"N_LIMIT": raw})
    assert Config.load(KEYS, {"N_LIMIT": "9223372036854775807"}).int("limit") == 2**63 - 1
    assert Config.load(KEYS, {"N_LIMIT": "-9223372036854775808"}).int("limit") == -(2**63)

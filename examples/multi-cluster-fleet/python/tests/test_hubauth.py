# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.

"""Hub credential tests.

The thing worth testing here is the refresh boundary. A token that never
refreshes works fine for an hour and then turns into a 401 storm that reads
like an RBAC problem, which is the most expensive way for this to fail.
"""

import pytest

from agent_sandbox_fleet import hubauth


class FakeClock:
  """Monotonic clock we can wind forward."""

  def __init__(self, now: float = 1000.0):
    self.now = now

  def __call__(self) -> float:
    return self.now


def _provider(payloads, clock):
  calls = []

  def fetch(url):
    calls.append(url)
    return payloads[min(len(calls) - 1, len(payloads) - 1)]

  p = hubauth.GkeMetadataTokenProvider(fetcher=fetch, clock=clock)
  return p, calls


def test_token_is_cached_between_calls():
  clock = FakeClock()
  p, calls = _provider([{"access_token": "t1", "expires_in": 3600}], clock)
  assert p.token() == "t1"
  assert p.token() == "t1"
  assert len(calls) == 1, "second call should not hit the metadata server"


def test_token_refreshes_before_it_actually_expires():
  # The whole point of the skew: refresh must happen while the old token is
  # still valid, not at the moment it dies.
  clock = FakeClock()
  p, calls = _provider(
      [{"access_token": "t1", "expires_in": 3600},
       {"access_token": "t2", "expires_in": 3600}], clock)
  assert p.token() == "t1"

  # One second before the refresh point (3600 - 300 skew).
  clock.now += 3600 - hubauth._EXPIRY_SKEW_S - 1
  assert p.token() == "t1"
  assert len(calls) == 1

  # One second past it, and still 299s before the token would truly expire.
  clock.now += 2
  assert p.token() == "t2"
  assert len(calls) == 2


def test_short_lived_token_still_refreshes_rather_than_pinning():
  # expires_in below the skew must not produce a non-positive validity window
  # that either divides by zero or caches forever.
  clock = FakeClock()
  p, calls = _provider(
      [{"access_token": "a", "expires_in": 60},
       {"access_token": "b", "expires_in": 60}], clock)
  assert p.token() == "a"
  clock.now += 2
  assert p.token() == "b"


def test_missing_expiry_falls_back_to_a_short_lifetime():
  clock = FakeClock()
  p, calls = _provider(
      [{"access_token": "a"}, {"access_token": "b"}], clock)
  assert p.token() == "a"
  clock.now += hubauth._DEFAULT_LIFETIME_S
  assert p.token() == "b"


def test_missing_access_token_names_the_likely_cause():
  clock = FakeClock()
  p, _ = _provider([{"expires_in": 3600}], clock)
  with pytest.raises(RuntimeError, match="Workload Identity"):
    p.token()


def test_unknown_token_source_is_rejected_before_any_io():
  with pytest.raises(ValueError, match="unknown token source"):
    hubauth.load_hub_configuration(token_source="magic")


# --------------------------------------------------------------------------- #
# The Configuration wiring. These exercise load_hub_configuration's override
# without a real kubeconfig by faking the kubernetes module's two entry points.
# --------------------------------------------------------------------------- #

class FakeConfiguration:
  """Stands in for kubernetes.client.Configuration, header assembly included.

  The header assembly is the point. An earlier version of this fake stored
  api_key/api_key_prefix and nothing else, so the tests could only assert that
  we had written the right things into the right dicts -- which we had, while
  the real client still put an unprefixed token on the wire and GKE answered
  401. Asserting on inputs cannot catch that; asserting on the header can.

  `scheme_keyed` selects which generation of the generated client to imitate:

    False  kubernetes<36 -- prefix looked up under the header name.
    True   kubernetes>=36 -- key looked up under the security scheme name with
           the header name as an alias, prefix under the scheme name ONLY.
  """

  scheme_keyed = False

  def __init__(self):
    self.api_key = {}
    self.api_key_prefix = {}
    self.refresh_api_key_hook = None
    self.host = "https://hub.example:443"

  def get_api_key_with_prefix(self, identifier, alias=None):
    if self.refresh_api_key_hook is not None:
      self.refresh_api_key_hook(self)
    key = self.api_key.get(
        identifier, self.api_key.get(alias) if alias is not None else None)
    if not key:
      return None
    prefix = self.api_key_prefix.get(identifier)
    return f"{prefix} {key}" if prefix else key

  def auth_settings(self):
    if self.scheme_keyed:
      value = self.get_api_key_with_prefix("BearerToken", alias="authorization")
    else:
      value = self.get_api_key_with_prefix("authorization")
    if value is None:
      return {}
    return {"BearerToken": {"type": "api_key", "in": "header",
                            "key": "authorization", "value": value}}


class SchemeKeyedConfiguration(FakeConfiguration):
  """kubernetes>=36 semantics -- the generation that broke this."""

  scheme_keyed = True


def _header(cfg):
  """The Authorization value the client would actually send."""
  return (cfg.auth_settings().get("BearerToken") or {}).get("value")


def _install_fake_kubernetes(monkeypatch, loaded=None, configuration=None):
  import sys
  import types

  client_mod = types.ModuleType("kubernetes.client")
  client_mod.Configuration = configuration or FakeConfiguration
  config_mod = types.ModuleType("kubernetes.config")

  def load_kube_config(config_file=None, context=None, client_configuration=None):
    if loaded is not None:
      loaded.append({"config_file": config_file, "context": context})
    # Stand in for a kubeconfig that carries credentials of its own, so we can
    # prove gke-metadata overrides them rather than merging with them.
    client_configuration.api_key["authorization"] = "stale-from-kubeconfig"

  config_mod.load_kube_config = load_kube_config

  k8s = types.ModuleType("kubernetes")
  k8s.client = client_mod
  k8s.config = config_mod
  monkeypatch.setitem(sys.modules, "kubernetes", k8s)
  monkeypatch.setitem(sys.modules, "kubernetes.client", client_mod)
  monkeypatch.setitem(sys.modules, "kubernetes.config", config_mod)


class StubProvider:
  """Yields the next token on each call, clamping at the last.

  Note this does NOT model GkeMetadataTokenProvider, which caches and returns
  the same token until the refresh boundary. Use it where only one token is in
  play; use MutableProvider anywhere the number of calls matters, since the
  refresh hook fires on every credential lookup and the count is not obvious.
  """

  def __init__(self, tokens):
    self.tokens = list(tokens)
    self.calls = 0

  def token(self):
    self.calls += 1
    return self.tokens[min(self.calls - 1, len(self.tokens) - 1)]


class MutableProvider:
  """Caches like the real provider; rotates only when the test says so."""

  def __init__(self, token):
    self.current = token
    self.calls = 0

  def token(self):
    self.calls += 1
    return self.current


def test_kubeconfig_source_leaves_credentials_alone(monkeypatch):
  loaded = []
  _install_fake_kubernetes(monkeypatch, loaded)
  cfg = hubauth.load_hub_configuration(
      kubeconfig="/etc/fleet-hub/kubeconfig", context="hub",
      token_source="kubeconfig")
  assert cfg.api_key["authorization"] == "stale-from-kubeconfig"
  assert cfg.refresh_api_key_hook is None
  assert loaded == [{"config_file": "/etc/fleet-hub/kubeconfig",
                     "context": "hub"}]


def test_gke_metadata_overrides_kubeconfig_credentials(monkeypatch):
  _install_fake_kubernetes(monkeypatch)
  provider = StubProvider(["first"])
  cfg = hubauth.load_hub_configuration(
      kubeconfig="/etc/fleet-hub/kubeconfig",
      token_source="gke-metadata", token_provider=provider)
  assert cfg.api_key["authorization"] == "first"
  assert cfg.api_key_prefix["authorization"] == "Bearer"
  assert _header(cfg) == "Bearer first"


@pytest.mark.parametrize("config_cls",
                         [FakeConfiguration, SchemeKeyedConfiguration],
                         ids=["header-keyed (k8s<36)", "scheme-keyed (k8s>=36)"])
def test_authorization_header_carries_the_bearer_prefix(monkeypatch, config_cls):
  # THE REGRESSION TEST. Under kubernetes>=36 the prefix is looked up by
  # security scheme name, so writing it only under 'authorization' produced a
  # header of a bare `ya29....` token. Every input assertion still passed; the
  # hub returned 401 Unauthorized, identical to an anonymous request, and the
  # error pointed at Workload Identity, which was fine all along.
  _install_fake_kubernetes(monkeypatch, configuration=config_cls)
  cfg = hubauth.load_hub_configuration(
      kubeconfig="/etc/fleet-hub/kubeconfig",
      token_source="gke-metadata", token_provider=StubProvider(["fake-gke-token"]))
  assert _header(cfg) == "Bearer fake-gke-token"


def test_a_prefixless_header_is_caught_at_startup(monkeypatch):
  # If a future client generation moves the lookup again, this must fail loudly
  # here rather than quietly at the hub. Simulate that by ignoring the prefix.
  class NoPrefixConfiguration(FakeConfiguration):
    def get_api_key_with_prefix(self, identifier, alias=None):
      if self.refresh_api_key_hook is not None:
        self.refresh_api_key_hook(self)
      return self.api_key.get("authorization")

  _install_fake_kubernetes(monkeypatch, configuration=NoPrefixConfiguration)
  with pytest.raises(RuntimeError, match="without its 'Bearer ' prefix"):
    hubauth.load_hub_configuration(
        kubeconfig="/etc/fleet-hub/kubeconfig",
        token_source="gke-metadata", token_provider=StubProvider(["fake-gke-token"]))


def test_refresh_survives_the_scheme_keyed_lookup(monkeypatch):
  # api_key is deliberately NOT mirrored under 'BearerToken': that copy would
  # win the lookup and pin the first token forever, which is the hourly-expiry
  # bug the refresh hook exists to prevent.
  _install_fake_kubernetes(monkeypatch, configuration=SchemeKeyedConfiguration)
  provider = MutableProvider("first")
  cfg = hubauth.load_hub_configuration(
      kubeconfig="/etc/fleet-hub/kubeconfig",
      token_source="gke-metadata", token_provider=provider)
  assert _header(cfg) == "Bearer first"
  provider.current = "second"
  assert _header(cfg) == "Bearer second"
  assert "BearerToken" not in cfg.api_key


def test_gke_metadata_still_loads_the_kubeconfig_for_server_and_ca(monkeypatch):
  # The credential-free ConfigMap is still the source of the hub address; if
  # this stopped being loaded the client would have no host to talk to.
  loaded = []
  _install_fake_kubernetes(monkeypatch, loaded)
  hubauth.load_hub_configuration(
      kubeconfig="/etc/fleet-hub/kubeconfig", context="hub",
      token_source="gke-metadata", token_provider=StubProvider(["x"]))
  assert loaded == [{"config_file": "/etc/fleet-hub/kubeconfig",
                     "context": "hub"}]


def test_refresh_hook_picks_up_a_rotated_token(monkeypatch):
  # Simulates what the client does on each request. Without the hook the
  # process would carry the startup token until it 401s an hour later.
  _install_fake_kubernetes(monkeypatch)
  provider = MutableProvider("first")
  cfg = hubauth.load_hub_configuration(
      token_source="gke-metadata", token_provider=provider)
  assert cfg.api_key["authorization"] == "first"
  provider.current = "second"
  cfg.refresh_api_key_hook(cfg)
  assert cfg.api_key["authorization"] == "second"

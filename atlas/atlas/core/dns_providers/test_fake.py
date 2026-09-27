from __future__ import annotations

from unittest.mock import MagicMock, patch

import frappe
from frappe.tests import UnitTestCase

from atlas.atlas.core.dns_providers.fake import FakeDnsProvider, FakeDnsProviderError


class _FakeSettings:
	wildcard_domain = "*.example.com"


def _build_provider() -> FakeDnsProvider:
	return FakeDnsProvider(settings=_FakeSettings())


class TestFakeDnsProvider(UnitTestCase):
	def test_validate_settings_rejects_production(self) -> None:
		provider = _build_provider()

		with patch.dict(frappe.conf, {"developer_mode": 0}):
			self.assertRaises(FakeDnsProviderError, provider.validate_settings)

	def test_validate_settings_allows_developer_mode(self) -> None:
		provider = _build_provider()

		with patch.dict(frappe.conf, {"developer_mode": 1}):
			provider.validate_settings()

	def test_validate_credentials_returns_true(self) -> None:
		provider = _build_provider()

		self.assertTrue(provider.validate_credentials())

	def test_create_zone_is_stable_for_the_same_domain(self) -> None:
		provider = _build_provider()

		self.assertEqual(provider.create_zone("example.com"), provider.create_zone("example.com"))

	def test_find_public_zone_id_matches_create_zone(self) -> None:
		provider = _build_provider()

		self.assertEqual(provider.find_public_zone_id("example.com"), provider.create_zone("example.com"))

	def test_bootstrap_marks_dns_setup_completed(self) -> None:
		provider = _build_provider()
		provider.settings.save = MagicMock()

		provider.bootstrap()

		self.assertEqual(provider.settings.is_dns_setup_completed, 1)
		provider.settings.save.assert_called_once()

	def test_create_https_health_check_returns_a_unique_id(self) -> None:
		provider = _build_provider()

		self.assertNotEqual(
			provider.create_https_health_check("203.0.113.9", "proxy.example.com", "/readyz"),
			provider.create_https_health_check("203.0.113.9", "proxy.example.com", "/readyz"),
		)

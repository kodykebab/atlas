from __future__ import annotations

from typing import override
from uuid import uuid4

import frappe

from atlas.atlas.core.dns_providers import register
from atlas.atlas.core.dns_providers.base import DnsProvider


class FakeDnsProviderError(Exception):
	"""Raised when the Fake DNS provider is selected outside developer mode."""


@register
class FakeDnsProvider(DnsProvider):
	"""Simulate a DNS provider for local development.

	`validate_settings` throws on a site without `developer_mode`, so this is
	inert on production.
	"""

	provider_type = "Fake"
	credential_fields = ()

	@override
	def validate_settings(self) -> None:
		"""Reject the Fake provider outside developer mode."""
		if not frappe.conf.get("developer_mode"):
			raise FakeDnsProviderError("The Fake DNS provider needs developer_mode.")

	@override
	def bootstrap(self) -> None:
		"""Mark DNS setup complete without a real zone or record."""
		domain = self.settings.wildcard_domain.removeprefix("*.")
		self.settings.route53_dns_zone_id = self.create_zone(domain)
		self.settings.is_dns_setup_completed = 1
		self.settings.save()

	@override
	def validate_credentials(self) -> bool:
		"""Return true because the Fake provider has no credentials to check."""
		return True

	@override
	def create_zone(self, domain: str) -> str:
		"""Return a stable synthetic zone ID for the domain."""
		return f"fake-zone-{domain}"

	@override
	def find_public_zone_id(self, domain: str) -> str | None:
		"""Return the synthetic zone ID, as if a public zone always exists."""
		return self.create_zone(domain)

	@override
	def upsert_record(self, record_type: str, name: str, values: list[str], ttl: int = 300) -> None:
		"""Do nothing. The Fake provider holds no real records."""

	@override
	def remove_record(self, record_type: str, name: str) -> None:
		"""Do nothing. The Fake provider holds no real records."""

	@override
	def create_https_health_check(self, ip_address: str, domain: str, path: str) -> str:
		"""Return a synthetic health check ID."""
		return f"fake-health-check-{uuid4()}"

	@override
	def remove_health_check(self, health_check_id: str) -> None:
		"""Do nothing. The Fake provider holds no real health checks."""

	@override
	def upsert_multivalue_a_record(
		self,
		name: str,
		identifier: str,
		ip_address: str,
		health_check_id: str,
		ttl: int = 120,
	) -> None:
		"""Do nothing. The Fake provider holds no real records."""

	@override
	def remove_multivalue_a_record(self, name: str, identifier: str) -> None:
		"""Do nothing. The Fake provider holds no real records."""

import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import yaml


class ExtractOpenAPITests(unittest.TestCase):
    def test_import_preserves_global_actor_and_separate_credentials(self):
        root = Path(__file__).resolve().parent.parent
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "imported.yaml"
            subprocess.run(
                [sys.executable, str(root / "scripts/extract_openapi.py"), str(root / "openapi.yaml"), str(output)],
                check=True, capture_output=True, text=True,
            )
            imported = yaml.safe_load(output.read_text())
        checked_in = yaml.safe_load((root / "openapi.yaml").read_text())
        schemes = imported["components"]["securitySchemes"]
        description = schemes["billingServiceAuth"]["description"]
        self.assertIn("X-Billing-Actor-Type=user", description)
        self.assertNotIn("X-Billing-Actor-Type=brand_cloud_user", description)
        self.assertEqual(schemes, checked_in["components"]["securitySchemes"])
        for path, method, scheme in (
            ("/v1/orgs/{orgId}/billing/account", "get", "billingServiceAuth"),
            ("/v1/internal/billing/access/{orgId}", "get", "billingInternalAuth"),
            ("/v1/internal/billing/debits", "post", "billingDebitAuth"),
        ):
            with self.subTest(path=path):
                self.assertEqual(imported["paths"][path][method]["security"], [{scheme: []}])


if __name__ == "__main__":
    unittest.main()

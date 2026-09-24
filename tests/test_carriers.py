import importlib.util
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location('carriers', Path(__file__).resolve().parents[1] / 'deploy/import-carriers.py')
carriers = importlib.util.module_from_spec(spec)
spec.loader.exec_module(carriers)


class CarrierImporterTests(unittest.TestCase):
    def test_constrained_selector_not_broadened(self):
        result = carriers.selectors({'SupportedSIMs': ['23410_GID1-508FFFFF'], 'SupportedPLMNs': ['23410']})
        self.assertEqual(result, [{'plmn': '23410', 'gid1': '508'}])

    def test_unknown_constraint_is_not_removed(self):
        self.assertEqual(carriers.selectors({'SupportedSIMs': ['23410_UNKNOWN-any'], 'SupportedPLMNs': ['23410']}), [])

    def test_no_disable_security_imported(self):
        result = carriers.convert('Example', {'SupportedSIMs': ['23410'], 'TechSettings': {'IKE': {'ValidateRemoteCertificate': False}}})
        self.assertEqual(set(result), {'id', 'match'})

    def test_unsupported_auth_explicit(self):
        result = carriers.convert('Example', {'SupportedSIMs': ['23410'], 'TechSettings': {'IKE': {'Proposals': [{'EAPMethod': 'EAP-TLS'}]}}})
        self.assertEqual(result['unsupported'], 'eap-method')

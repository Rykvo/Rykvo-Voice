"""Extract interoperable facts from local carrier bundles; never execute bundle code."""
import argparse
import hashlib
import json
import plistlib
import re
import tarfile
from pathlib import Path


def selectors(document):
    result = []
    for value in document.get('SupportedSIMs', []):
        if not isinstance(value, str):
            continue
        parts = value.split('_')
        if not re.fullmatch(r'\d{5,6}', parts[0]):
            continue
        match = {'plmn': parts[0]}
        valid = True
        for part in parts[1:]:
            key, sep, val = part.partition('-')
            key = key.lower()
            if not sep or key not in ('gid1', 'gid2', 'spn', 'iccid'):
                valid = False
                break
            if key.startswith('gid'):
                val = re.sub(r'F+$', '', val.upper())
                if not val or not re.fullmatch('[0-9A-F]+', val):
                    valid = False
            match[key] = val
        if valid and match not in result:
            result.append(match)
    # Never broaden a constrained MVNO to its host PLMN.
    if not document.get('SupportedSIMs'):
        for value in document.get('SupportedPLMNs', []):
            if isinstance(value, str) and re.fullmatch(r'\d{5,6}', value):
                result.append({'plmn': value})
    return result


def convert(name, document):
    matches = selectors(document)
    tech = document.get('TechSettings', {})
    if not matches or not isinstance(tech, dict):
        return None
    ike = tech.get('IKE', {})
    rule = {'id': re.sub('[^a-z0-9-]+', '-', name.lower()).strip('-'), 'match': matches}
    host = ike.get('RemoteAddress', '').lower()
    if re.fullmatch(r'epdg[\w.-]*\.[a-z]{2,}', host) and '$' not in host:
        rule['epdg'] = host
    # Unsupported authentication is explicit, not silently mapped to AKA.
    methods = {p.get('EAPMethod') for p in ike.get('Proposals', []) if isinstance(p, dict)}
    if methods and 'EAP-AKA' not in methods:
        rule['unsupported'] = 'eap-method'
    def walk(v):
        if isinstance(v, dict):
            for k, x in v.items():
                if k == 'CountryOfOriginationFormat' and x == 'PANI':
                    rule['country'] = 'AUTO'
                if k == 'SignalingTransport' and str(x).lower() in ('tcp', 'udp'):
                    rule['transport'] = str(x).lower()
                walk(x)
        elif isinstance(v, list):
            for x in v:
                walk(x)
    walk(document.get('IMSConfig', {}))
    return rule


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('archive', type=Path)
    p.add_argument('output', type=Path)
    args = p.parse_args()
    rules = []
    with tarfile.open(args.archive, 'r:*') as archive:
        for index, member in enumerate(archive):
            if index > 30000:
                raise ValueError('archive has too many entries')
            if not member.isfile() or not member.name.endswith('/carrier.plist'):
                continue
            if '/Carrier Bundles/' not in member.name or member.size > 4 * 1024 * 1024:
                continue
            document = plistlib.loads(archive.extractfile(member).read())
            rule = convert(Path(member.name).parent.name.removesuffix('.bundle'), document)
            if rule:
                rules.append(rule)
    rules.sort(key=lambda x: x['id'])
    result = {'version': 1, 'source': 'ios-carrier-bundles carrier.plist interoperability facts',
              'sha256': hashlib.sha256(args.archive.read_bytes()).hexdigest(), 'profiles': rules}
    args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(f'{len(rules)} profiles; importing data does not verify network support')


if __name__ == '__main__':
    main()

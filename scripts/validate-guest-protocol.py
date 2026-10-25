"""校验候选协议 Schema、正反例与操作参数指纹；需要 Python jsonschema。"""

import hashlib
import json
from pathlib import Path

from jsonschema import Draft202012Validator


# main 校验候选契约及其 fixture，不启动 VM 或修改生产代码。
def main():
    root = Path(__file__).resolve().parent.parent
    schema = json.loads((root / 'design/schemas/guest-protocol-v0.1.schema.json').read_text())
    Draft202012Validator.check_schema(schema)
    cases = json.loads((root / 'design/fixtures/guest-protocol-v0.1.json').read_text())
    for case in cases:
        assert case['definition'] in schema['$defs'], case['name']
        validator = Draft202012Validator({
            '$schema': schema['$schema'],
            '$defs': schema['$defs'],
            '$ref': '#/$defs/' + case['definition'],
        })
        errors = list(validator.iter_errors(case['payload']))
        assert (not errors) == case['valid'], (case['name'], [error.message for error in errors])

    vectors = json.loads((root / 'design/fixtures/guest-operation-v0.1.json').read_text())
    fields = {
        'PrepareAgent': ['manifestRef', 'manifestDigest', 'configDigest', 'workspaceRef', 'workspaceDigest'],
        'StartSession': ['sessionID', 'preparedID', 'workspaceRef', 'workspaceDigest', 'rows', 'cols'],
    }
    for vector in vectors:
        method = vector['method']
        params = vector['params']
        validator = Draft202012Validator({
            '$defs': schema['$defs'], '$ref': '#/$defs/' + method + 'Params',
        })
        validator.validate(params)
        canonical = json.dumps([method] + [params[key] for key in fields[method]], separators=(',', ':'))
        assert canonical == vector['canonical'], method
        assert hashlib.sha256(canonical.encode('utf-8')).hexdigest() == vector['sha256'], method
    print(f'Schema、{len(cases)} 个负载正反例、{len(vectors)} 个参数指纹向量通过')


if __name__ == '__main__':
    main()

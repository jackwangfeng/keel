"""gen_dart_schema 的单测：用一份小 spec 夹具覆盖映射规则。运行：python -m unittest scripts/test_gen_dart_schema.py"""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(__file__))
import gen_dart_schema as g  # noqa: E402

SPEC = {
    'paths': {},
    'components': {
        'parameters': {},
        'schemas': {
            'Money': {'type': 'integer', 'description': '分'},
            'Item': {
                'type': 'object',
                'required': ['id', 'price_cents', 'tags', 'note'],
                'properties': {
                    'id': {'type': 'integer'},
                    'price_cents': {'$ref': '#/components/schemas/Money'},
                    'weight': {'type': 'number'},
                    'tags': {'type': 'array', 'items': {'type': 'string'}},
                    'note': {'type': ['string', 'null']},
                    'spec_values': {'type': 'object', 'additionalProperties': {'type': 'string'}},
                    'owner': {'type': 'object', 'properties': {'name': {'type': 'string'}}},
                    'default': {'type': 'boolean'},
                    'anything': {'description': '没写 type 的字段'},
                },
            },
            'Page': {'allOf': [
                {'type': 'object', 'required': ['total'], 'properties': {'total': {'type': 'integer'}}},
                {'type': 'object', 'required': ['items'], 'properties': {
                    'items': {'type': 'array', 'items': {'$ref': '#/components/schemas/Item'}}}},
            ]},
        },
    },
}


class GenDartTest(unittest.TestCase):
    def setUp(self):
        self.out = g.render(SPEC, operations=[])

    def test_scalar_alias(self):
        self.assertIn('typedef Money = int;', self.out)

    def test_required_and_optional_fields(self):
        self.assertIn('final int id;', self.out)
        self.assertIn('final Money priceCents;', self.out)
        self.assertIn('final double? weight;', self.out)
        self.assertIn('final List<String> tags;', self.out)
        self.assertIn('final String? note;', self.out)
        self.assertIn('final Map<String, String>? specValues;', self.out)

    def test_dynamic_not_marked_nullable(self):
        # dynamic 本身就可空，写成 dynamic? 是 lint 警告
        self.assertIn('final dynamic anything;', self.out)
        self.assertNotIn('dynamic?', self.out)

    def test_inline_object_promoted(self):
        self.assertIn('class ItemOwner {', self.out)
        self.assertIn('final ItemOwner? owner;', self.out)

    def test_reserved_word_escaped(self):
        self.assertIn('final bool? default_;', self.out)
        self.assertIn("'default': default_", self.out)

    def test_number_tolerance(self):
        # 整数字段收到 12.0、浮点字段收到 12 都要能进
        self.assertIn("(j['id'] as num).toInt()", self.out)
        self.assertIn("(j['weight'] as num?)?.toDouble()", self.out)

    def test_nullable_required_written_even_if_null(self):
        # type [X, null] 且 required：toJson 恒写这个键；非 required 为 null 时不写
        self.assertIn("'note': note,", self.out)
        self.assertIn("if (weight != null) 'weight': weight,", self.out)

    def test_all_of_flattened(self):
        self.assertIn('class Page {', self.out)
        self.assertIn('final int total;', self.out)
        self.assertIn('final List<Item> items;', self.out)
        self.assertIn("(j['items'] as List).map((e) => Item.fromJson(e as Map<String, dynamic>)).toList()", self.out)

    def test_unknown_operation_fails(self):
        with self.assertRaises(SystemExit):
            g.render(SPEC, operations=[('get', '/nope', 'Nope')])


if __name__ == '__main__':
    unittest.main()

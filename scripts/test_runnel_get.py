import runpy
import unittest
from pathlib import Path


CLIENT = runpy.run_path(str(Path(__file__).with_name("runnel-get")))


class MarkdownConversionTests(unittest.TestCase):
    def test_removes_noise_and_preserves_structure(self):
        html = b"""<!doctype html><html><head><style>.x{}</style><script>bad()</script></head>
        <body><nav>Menu</nav><main><h1>Hello &amp; world</h1><p>Useful <strong>content</strong>.</p>
        <ul><li>One</li><li>Two</li></ul><pre>  keep\n    indent</pre></main><footer>Noise</footer></body></html>"""

        result = CLIENT["html_to_markdown"](html).decode()

        self.assertEqual(
            result,
            "# Hello & world\n\nUseful content.\n- One\n- Two\n\n```\n  keep\n    indent\n```\n",
        )
        self.assertNotIn("bad()", result)
        self.assertNotIn("Menu", result)
        self.assertNotIn("Noise", result)

    def test_replaces_invalid_bytes(self):
        result = CLIENT["html_to_markdown"](b"<p>hello \xff</p>", "utf-8").decode()
        self.assertEqual(result, "hello \ufffd\n")


if __name__ == "__main__":
    unittest.main()

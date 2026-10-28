import unittest

import infra_graph


def valid_graph():
    graph = {}
    for dependent, prerequisite in infra_graph.REQUIRED_PATHS:
        graph.setdefault(dependent, set()).add(prerequisite)
        graph.setdefault(prerequisite, set())
    return graph


def dot(graph):
    lines = ["digraph G {", '  rankdir = "RL";', '  node [shape = rect, fontname = "sans-serif"];']
    lines.extend(f'  "{node}" [label="{node}"];' for node in graph)
    lines.extend(f'  "{node}" -> "{dependency}";' for node, dependencies in graph.items() for dependency in dependencies)
    return "\n".join(lines + ["}"])


class BootstrapGraphTests(unittest.TestCase):
    def test_parse_and_valid_bootstrap(self):
        graph = valid_graph()
        self.assertEqual(infra_graph.parse_graph(dot(graph)), graph)
        infra_graph.validate_bootstrap(graph)

    def test_transitive_dependencies_are_sufficient(self):
        graph = valid_graph()
        # Node -> CNI -> agent already supplies the dependency Terraform reduces.
        graph["aws_eks_node_group.lab"].remove("aws_eks_addon.pod_identity_agent")
        infra_graph.validate_bootstrap(graph)

    def test_each_required_path_is_enforced(self):
        for dependent, prerequisite in infra_graph.REQUIRED_PATHS:
            with self.subTest(dependent=dependent, prerequisite=prerequisite):
                graph = valid_graph()
                # Cut every route into the prerequisite, including indirect ones.
                for dependencies in graph.values():
                    dependencies.discard(prerequisite)
                with self.assertRaisesRegex(ValueError, "must depend"):
                    infra_graph.validate_bootstrap(graph)

    def test_agent_waiting_for_nodes_creates_a_cycle(self):
        graph = valid_graph()
        graph["aws_eks_addon.pod_identity_agent"].add("aws_eks_node_group.lab")
        with self.assertRaisesRegex(ValueError, "cycle"):
            infra_graph.validate_bootstrap(graph)

    def test_missing_resource_is_not_success(self):
        graph = valid_graph()
        del graph["aws_eks_node_group.lab"]
        with self.assertRaisesRegex(ValueError, "Missing bootstrap resource"):
            infra_graph.validate_bootstrap(graph)

    def test_rejects_empty_truncated_and_unexpected_formats(self):
        for text in ("", "digraph G {\n}", dot(valid_graph())[:-1],
                     'digraph G {\n"a" -> "b";\n}',
                     'digraph G {\n"a" -> "b" -> "c";\n}',
                     'digraph G {\nunsupported;\n}'):
            with self.subTest(text=text), self.assertRaises(ValueError):
                infra_graph.parse_graph(text)


if __name__ == "__main__":
    unittest.main()

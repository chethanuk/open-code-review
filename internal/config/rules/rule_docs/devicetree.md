#### Devicetree Review Principles
> Report nothing if the file under review is not a Devicetree source (`/dts-v1/;` header, or `.dtsi` include fragment containing nodes and properties). Favor precision over recall: only raise an issue when you are confident it is a real defect, and stay silent when the surrounding context is unclear. Ground findings in the diff and in observable parent nodes, included `.dtsi` files, and binding documents. Do not assume the binding schema, the kernel or Zephyr version, or the provider's `#*-cells` values when they are not visible. Do not repeat what `dtc` already reports as a warning or error. A `.dtsi` may be overridden by the `.dts` that includes it, so judge a property in context of the including file when it is visible.

#### Obvious Typos or Spelling Errors
- Spelling errors in node names, labels, and property names at their declaration sites; do not report spelling errors at reference sites
- Typos in string values such as `status`, `compatible`, or `pinctrl-names` that make the node or driver not match

#### Node Addressing and Registers
- A unit-address in the node name (`node@1000`) that does not match the first address in its `reg` property
- A `reg` property whose cell count is inconsistent with the parent node's `#address-cells` and `#size-cells`
- Overlapping `reg` ranges between sibling nodes, or a `reserved-memory` region overlapping another region or the memory node
- `ranges` or `dma-ranges` that translate a child address space to the wrong parent window or size

#### Compatible, Status and Overrides
- A `compatible` list not ordered from most specific to most generic, or missing the fallback string that the binding requires
- `status = "disabled"` left on a node the board depends on, or `status = "okay"` on a node whose required supplies, clocks, or pins are not described
- `/delete-node/`, `/delete-property/`, or a `&label { ... }` override that references a label which does not exist in the including files
- Include order in which a later `#include` or `/include/` silently overrides properties the author intended to keep

#### Phandles, Interrupts and Resources
- A phandle property (`clocks`, `resets`, `gpios`, `dmas`, `interrupts-extended`, and similar) whose argument count does not match the provider's `#clock-cells`, `#reset-cells`, `#gpio-cells`, `#dma-cells`, or `#interrupt-cells`
- Interrupt specifiers with the wrong trigger type or level flag, or a missing or incorrect `interrupt-parent` where the inherited parent is not the intended controller
- `*-names` properties whose entries do not line up one to one with the corresponding `clocks`, `resets`, `dmas`, or `interrupts` entries
- `pinctrl-N` and `pinctrl-names` mismatches, such as a state name the driver expects (`default`, `sleep`) being absent
- An enabled device node that references a regulator, clock, or GPIO label that is not defined

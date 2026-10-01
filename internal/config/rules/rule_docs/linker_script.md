#### Linker Script Review Principles
> Favor precision over recall: only raise an issue when you are confident it is a real defect, and stay silent when the surrounding context is unclear. Linker scripts (`.ld`, `.lds`, and preprocessed `.lds.S`) may use the C preprocessor; do not assume the toolchain, target architecture, or memory map beyond what the diff and surrounding file show. If the file is plainly assembly source rather than a linker script, or is not a linker script at all, report nothing.

#### Obvious Typos or Spelling Errors
- Spelling errors in section, symbol, or memory-region names at their declaration sites; do not report spelling errors at reference sites
- Section or region names that differ by a typo from the names used by the code or other parts of the script

#### Memory Regions and Placement
- `MEMORY` regions that overlap, or that are too small for the sections assigned to them
- Load address (`AT>` / `AT()`) and virtual address mismatches for data that is copied or relocated at startup
- Section ordering that moves an entry point, vector table, or image header away from the position its consumer requires

#### Symbols, Alignment and Retention
- Missing or insufficient `ALIGN` for sections whose consumers need alignment (page tables, stacks, vector tables)
- Start/end symbols (such as a `.bss` start or end) defined outside the section they are meant to bound
- Sections referenced only by the loader or init tables lacking `KEEP()`, so they may be dropped by `--gc-sections`
- Location-counter (`.`) assignments that overlap earlier output or silently waste space
- `ASSERT` checks or `ENTRY` declarations removed without replacement

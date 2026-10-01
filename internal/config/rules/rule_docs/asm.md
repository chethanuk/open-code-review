#### Assembly Review Principles
> Favor precision over recall: only raise an issue when you are confident it is a real defect, and stay silent when the surrounding context is unclear. Assume the target architecture (ARM/AArch64, RISC-V, x86, MIPS, etc.), assembler syntax, and calling convention only from evidence in the diff or surrounding file; do not apply one architecture's rules to another. Do not repeat what the assembler already diagnoses. If the file is not assembly source (the `.s` extension is occasionally used for other formats), report nothing.

#### Obvious Typos or Spelling Errors
- Spelling errors in label, symbol, or macro names at their declaration sites; do not report spelling errors at reference sites
- Typos in comments or string directives (`.ascii`, `.asciz`) that affect readability

#### Calling Convention and Register Use
- Callee-saved registers modified without being saved and restored, per the calling convention evident in the file
- Argument or return-value registers overwritten before they are read
- Stack pointer alignment violated at a call boundary, or push/pop (or stack adjustment) sequences that are unbalanced on some path
- Link register or return address lost across a nested call without being saved
- Use of memory below the stack pointer where the convention does not permit it

#### Memory Ordering and Privileged State
- Missing barriers (such as `dmb`/`dsb`/`isb`, `fence`, `mfence`) after MMIO accesses, cache or TLB maintenance, or system-register writes whose effect later code depends on
- Interrupts or exceptions enabled before the stack pointer or exception vectors are set up
- Symbol annotations (`.type`, `.size`, project-specific function-end macros) missing only where the surrounding code clearly uses them consistently

#### Sections, Alignment and Macros
- Code or data placed in the wrong section (for example runtime code in an init-only section)
- Missing or insufficient alignment for vector tables, page-aligned data, or instructions that require it
- Macro arguments that are expanded more than once, so side effects or register clobbers repeat
- Conditional-assembly branches that leave the stack, labels, or sections unbalanced

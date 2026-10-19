//go:build amd64 && !purego

#include "textflag.h"

// ABI0: string (base at 0, len at 8), byte (16), bool return (24).
TEXT ·asmContainsByte(SB), NOSPLIT, $0-25
    MOVQ s_base+0(FP), DI
    MOVQ s_len+8(FP), CX
    MOVBLZX needle+16(FP), AX
    MOVD AX, X0
    PUNPCKLBW X0, X0
    PUNPCKLBW X0, X0
    PSHUFD $0, X0, X0
vector_loop:
    CMPQ CX, $16
    JB tail
    MOVOU (DI), X1
    PCMPEQB X0, X1
    PMOVMSKB X1, AX
    TESTL AX, AX
    JNE found
    ADDQ $16, DI
    SUBQ $16, CX
    JMP vector_loop
tail:
    TESTQ CX, CX
    JE missing
    MOVBLZX (DI), AX
    CMPB AL, needle+16(FP)
    JE found
    INCQ DI
    DECQ CX
    JMP tail
found:
    MOVB $1, ret+24(FP)
    RET
missing:
    MOVB $0, ret+24(FP)
    RET

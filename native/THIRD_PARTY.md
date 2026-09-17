# Native dependencies

The native GUI statically links wxWidgets 3.2.8, licensed under the wxWindows Library Licence, which permits distributing proprietary or open-source applications linked with wxWidgets. Official license: https://github.com/wxWidgets/wxWidgets/blob/v3.2.8/docs/licence.txt . The build downloads the official release source archive and verifies its SHA-256 digest.

wxWidgets' bundled zlib and libpng are built from the same source archive. Their notices are in `src/zlib/README` and `src/png/LICENSE` within that archive. Release packaging must include these license texts and wxWidgets' `docs/licence.txt`.

The LLVM-MinGW validation toolchain links libc++, libc++abi, compiler-rt and MinGW support statically; retain the toolchain's root `LICENSE.TXT` and distribution license notices when distributing builds made with it. Compiler tools themselves are not required in the offline application package. The executable imports only Windows system libraries.

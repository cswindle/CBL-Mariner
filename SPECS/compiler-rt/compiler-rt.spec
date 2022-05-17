%global maj_ver 12
%global min_ver 0
%global patch_ver 1

%global compiler_rt_srcdir %{name}-%{version}.src

Summary:        LLVM compiler runtime libraries
Name:           compiler-rt
Version:        %{maj_ver}.%{min_ver}.%{patch_ver}
Release:        1%{?dist}
License:        NCSA or MIT
Vendor:         Microsoft Corporation
Distribution:   Mariner
Group:          Development/Tools
URL:            https://clang.llvm.org
Source0:        https://github.com/llvm/llvm-project/releases/download/llvmorg-%{version}/%{compiler_rt_srcdir}.tar.xz
BuildRequires:  gcc
BuildRequires:  gcc-c++
BuildRequires:  cmake
BuildRequires:  python3
BuildRequires:  llvm-devel = %{version}
BuildRequires:  sed
BuildRequires:  make

%description
The compiler-rt project consists of the following runtime libraries:
- builtins - provides highly tuned libgcc implementations
- sanitizer runtimes - runtime libraries that are required to run the code with sanitizer instrumentation
- profile - library which is used to collect coverage information
- BlocksRuntime - a target-independent implementation of Apple "Blocks" runtime interfaces

%prep

%setup -q -n %{compiler_rt_srcdir}

%build
# Disable symbol generation
export CFLAGS="`echo " %{build_cflags} " | sed 's/ -g//'`"
export CXXFLAGS="`echo " %{build_cxxflags} " | sed 's/ -g//'`"

mkdir -p build
cd build
cmake -DCMAKE_INSTALL_PREFIX=%{_prefix}   \
      -DCMAKE_BUILD_TYPE=Release    \
      -DLLVM_ENABLE_EH=ON \
      -DLLVM_ENABLE_RTTI=ON \
      -Wno-dev ..

%make_build

%install
cd build

%make_install

mkdir -p %{buildroot}%{_libdir}/clang/%{version}/lib/linux
for lib in $(ls %{buildroot}%{_libdir}/linux)
do
  ln -s %{_libdir}/linux/$lib %{buildroot}%{_libdir}/clang/%{version}/lib/linux
done

%post   -p /sbin/ldconfig
%postun -p /sbin/ldconfig

%check

%files
%defattr(-,root,root)
%license LICENSE.TXT
%{_includedir}/*
%{_bindir}/*
%{_libdir}/*
%{_datadir}/*

%changelog
* Mon May 16 2022 Chris Swindle <chrisswindle@microsoft.com> - 12.0.1-1
- Initial build.

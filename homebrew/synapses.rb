# Homebrew formula for Synapses — graph-based code intelligence MCP server.
#
# To use this formula, add the tap first:
#   brew tap SynapsesOS/tap
#   brew install synapses
#
# Or install directly:
#   brew install SynapsesOS/tap/synapses

class Synapses < Formula
  desc "Graph-based code intelligence MCP server with 24 tools, 18-language support"
  homepage "https://github.com/SynapsesOS/synapses"
  license "MIT"

  # Version is updated automatically by the release workflow.
  version "0.7.0"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/SynapsesOS/synapses/releases/download/v#{version}/synapses_darwin_arm64.tar.gz"
      # sha256 "UPDATE_ON_RELEASE"
    else
      url "https://github.com/SynapsesOS/synapses/releases/download/v#{version}/synapses_darwin_x86_64.tar.gz"
      # sha256 "UPDATE_ON_RELEASE"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/SynapsesOS/synapses/releases/download/v#{version}/synapses_linux_arm64.tar.gz"
      # sha256 "UPDATE_ON_RELEASE"
    else
      url "https://github.com/SynapsesOS/synapses/releases/download/v#{version}/synapses_linux_x86_64.tar.gz"
      # sha256 "UPDATE_ON_RELEASE"
    end
  end

  def install
    bin.install "synapses"
  end

  def caveats
    <<~EOS
      Quick start:
        cd your-project
        synapses init
        synapses start

      Add to Claude Code:
        claude mcp add synapses -- synapses start --path .

      Optional sidecars (not required):
        brew install SynapsesOS/tap/brain   # AI enrichment
        pip install synapses-scout          # web intelligence
    EOS
  end

  test do
    assert_match "synapses", shell_output("#{bin}/synapses version")
  end
end

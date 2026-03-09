# Homebrew formula for Brain — semantic enrichment sidecar for Synapses.
#
# To use this formula, add the tap first:
#   brew tap SynapsesOS/tap
#   brew install brain
#
# Or install directly:
#   brew install SynapsesOS/tap/brain

class Brain < Formula
  desc "Semantic enrichment sidecar for Synapses — 4-tier local LLM system"
  homepage "https://github.com/SynapsesOS/synapses-intelligence"
  license "MIT"

  # Version is updated automatically by the release workflow.
  version "0.7.0"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/SynapsesOS/synapses-intelligence/releases/download/v#{version}/brain_darwin_arm64.tar.gz"
      # sha256 "UPDATE_ON_RELEASE"
    else
      url "https://github.com/SynapsesOS/synapses-intelligence/releases/download/v#{version}/brain_darwin_x86_64.tar.gz"
      # sha256 "UPDATE_ON_RELEASE"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/SynapsesOS/synapses-intelligence/releases/download/v#{version}/brain_linux_arm64.tar.gz"
      # sha256 "UPDATE_ON_RELEASE"
    else
      url "https://github.com/SynapsesOS/synapses-intelligence/releases/download/v#{version}/brain_linux_x86_64.tar.gz"
      # sha256 "UPDATE_ON_RELEASE"
    end
  end

  def install
    bin.install "brain"
  end

  def caveats
    <<~EOS
      Setup (downloads a small LLM model):
        brain setup --llama-server
        brain serve

      Then configure synapses to use it:
        Add {"brain": {"url": "http://localhost:11435"}} to synapses.json
    EOS
  end

  test do
    assert_match "brain", shell_output("#{bin}/brain version")
  end
end

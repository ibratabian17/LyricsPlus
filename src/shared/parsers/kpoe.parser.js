/**
 * Normalizes a v2 lyrics object so every line element uses songPartIndex
 * pointing into metadata.songParts[], replacing legacy songPart strings.
 * If all lines already have songPartIndex, returns the object unchanged.
 */
export function normalizeV2(data) {
  const lyrics = Array.isArray(data?.lyrics) ? data.lyrics : [];
  if (lyrics.length === 0 || lyrics.every(l => l.element?.songPartIndex != null)) {
    return data;
  }

  const existingPartCount = Array.isArray(data.metadata?.songParts) ? data.metadata.songParts.length : 0;
  const songParts = (data.metadata?.songParts || []).map(part => ({ ...part }));
  let currentPartName = null;
  let currentPartIndex = -1;

  const normalizedLyrics = lyrics.map(line => {
    if (line.element?.songPartIndex != null) {
      currentPartName = null;
      return line;
    }
    const partName = line.element?.songPart || '';

    if (partName !== currentPartName) {
      currentPartName = partName;
      currentPartIndex = songParts.length;
      songParts.push({ name: partName });
    }

    const { songPart, ...restElement } = line.element || {};
    return {
      ...line,
      element: { ...restElement, songPartIndex: currentPartIndex }
    };
  });

  // Derive time/duration for each songPart from the lines that belong to it
  normalizedLyrics.forEach(line => {
    const idx = line.element.songPartIndex;
    const part = songParts[idx];
    if (!part || idx < existingPartCount) return;
    const endTime = line.time + line.duration;

    if (part.time == null || line.time < part.time) part.time = line.time;
    if (part._end == null || endTime > part._end) part._end = endTime;
  });

  songParts.forEach(part => {
    if (part.time != null && part._end != null) part.duration = part._end - part.time;
    delete part._end;
  });

  return {
    ...data,
    metadata: { ...data.metadata, songParts },
    lyrics: normalizedLyrics,
  };
}

export function v1Tov2(data) {
  const groupedLyrics = [];
  let currentGroup = null;

  if (data.type === "Line") {
    data.lyrics.forEach(segment => {
      groupedLyrics.push({
        time: segment.time,
        duration: segment.duration,
        text: segment.text,
        syllabus: [],
        element: segment.element || { key: "", songPart: "", singer: "" }
      });
    });
  } else {
    data.lyrics.forEach(segment => {
      if (!currentGroup) {
        currentGroup = {
          time: segment.time,
          duration: 0,
          text: "",
          syllabus: [],
          element: segment.element || { key: "", songPart: "", singer: "" }
        };
      }

      currentGroup.text += segment.text;

      const syllabusEntry = {
        time: segment.time,
        duration: segment.duration,
        text: segment.text
      };

      if (segment.element?.isBackground === true) {
        syllabusEntry.isBackground = true;
      }

      currentGroup.syllabus.push(syllabusEntry);

      if (segment.isLineEnding === 1) {
        let earliestTime = Infinity;
        let latestEndTime = 0;
        currentGroup.syllabus.forEach(syl => {
          if (syl.time < earliestTime) earliestTime = syl.time;
          const end = syl.time + syl.duration;
          if (end > latestEndTime) latestEndTime = end;
        });
        currentGroup.time = earliestTime;
        currentGroup.duration = latestEndTime - earliestTime;
        currentGroup.text = currentGroup.text.trim();
        groupedLyrics.push(currentGroup);
        currentGroup = null;
      }
    });

    if (currentGroup) {
      let earliestTime = Infinity;
      let latestEndTime = 0;
      currentGroup.syllabus.forEach(syl => {
        if (syl.time < earliestTime) earliestTime = syl.time;
        const end = syl.time + syl.duration;
        if (end > latestEndTime) latestEndTime = end;
      });
      currentGroup.time = earliestTime;
      currentGroup.duration = latestEndTime - earliestTime;
      currentGroup.text = currentGroup.text.trim();
      groupedLyrics.push(currentGroup);
    }
  }

  // normalizeV2 converts element.songPart -> element.songPartIndex
  // and builds metadata.songParts from the grouped lines
  return normalizeV2({
    type: data.type == "syllable" ? "Word" : data.type,
    KpoeTools: '2.0-LPlusBcknd,' + data.KpoeTools,
    metadata: data.metadata,
    ignoreSponsorblock: data.ignoreSponsorblock || undefined,
    lyrics: groupedLyrics,
    cached: data.cached || 'None'
  });
}

export function v2Tov1(data) {
  if (data.lyrics?.length > 0 && typeof data.lyrics[0].syllabus === 'undefined') {
    console.warn("Data is already in V1 format. No conversion needed.");
    return data;
  }

  const songPartsArray = data.metadata?.songParts || [];

  // Resolve songPart string from either format so v1 element always has songPart
  const resolveSongPart = (element) => {
    if (element?.songPart) return element.songPart;
    if (element?.songPartIndex != null && songPartsArray[element.songPartIndex]) {
      return songPartsArray[element.songPartIndex].name;
    }
    return '';
  };

  const flatLyrics = [];

  if (data.type === "Line") {
    data.lyrics.forEach(line => {
      const { songPartIndex, ...restElement } = line.element || {};
      flatLyrics.push({
        time: line.time,
        duration: line.duration,
        text: line.text,
        isLineEnding: 1,
        element: { ...restElement, songPart: resolveSongPart(line.element) }
      });
    });
  } else {
    data.lyrics.forEach(line => {
      const { songPartIndex, ...restElement } = line.element || {};
      const resolvedElement = { ...restElement, songPart: resolveSongPart(line.element) };

      if (!line.syllabus || line.syllabus.length === 0) {
        flatLyrics.push({
          time: line.time,
          duration: line.duration,
          text: line.text,
          isLineEnding: 1,
          element: resolvedElement
        });
        return;
      }

      line.syllabus.forEach((syllable, index) => {
        const v1Segment = {
          time: syllable.time,
          duration: syllable.duration,
          text: syllable.text,
          isLineEnding: index === line.syllabus.length - 1 ? 1 : 0,
          element: { ...resolvedElement }
        };
        if (syllable.isBackground) v1Segment.element.isBackground = true;
        flatLyrics.push(v1Segment);
      });
    });
  }

  return {
    type: data.type === "Word" ? "syllable" : data.type,
    KpoeTools: `2.0-V2toV1,${data.KpoeTools}`,
    metadata: data.metadata,
    ignoreSponsorblock: data.ignoreSponsorblock,
    lyrics: flatLyrics,
    cached: data.cached || 'None'
  };
}
